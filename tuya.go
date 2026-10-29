package main

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Tuya LAN protocol 3.3, which the AutoVac uses: every frame is
//
//	000055AA | seq | cmd | length | payload | crc32 | 0000AA55
//
// with the JSON payload AES-128-ECB encrypted under the device's local key.
// Frames from the device start their payload with a 4-byte return code.

const (
	framePrefix = 0x000055AA
	frameSuffix = 0x0000AA55

	cmdControl   = 7  // set data points
	cmdStatus    = 8  // device pushes changed data points
	cmdHeartbeat = 9  // keep the connection open
	cmdDPQuery   = 10 // read every data point

	tuyaPort = 6668
)

// version33Header goes in front of encrypted CONTROL payloads (but not queries
// or heartbeats), and in front of some of the device's pushes.
var version33Header = append([]byte("3.3"), make([]byte, 12)...)

type tuyaFrame struct {
	Seq     uint32
	Cmd     uint32
	RetCode uint32
	Payload []byte // decrypted JSON, or empty for a bare ack
}

type tuyaCodec struct {
	deviceID string
	key      []byte
}

func newTuyaCodec(deviceID, localKey string) (*tuyaCodec, error) {
	if len(localKey) != 16 {
		return nil, fmt.Errorf("local key must be 16 characters, got %d", len(localKey))
	}
	return &tuyaCodec{deviceID: deviceID, key: []byte(localKey)}, nil
}

// Encode builds a frame for cmd. dps is only used for CONTROL.
func (c *tuyaCodec) Encode(seq, cmd uint32, dps map[string]any) ([]byte, error) {
	t := strconv.FormatInt(time.Now().Unix(), 10)
	var body map[string]any
	switch cmd {
	case cmdControl:
		body = map[string]any{"devId": c.deviceID, "uid": c.deviceID, "t": t, "dps": dps}
	case cmdDPQuery:
		body = map[string]any{"gwId": c.deviceID, "devId": c.deviceID, "uid": c.deviceID, "t": t}
	case cmdHeartbeat:
		body = map[string]any{"gwId": c.deviceID, "devId": c.deviceID}
	default:
		return nil, fmt.Errorf("unsupported command %d", cmd)
	}
	plain, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	payload := ecbEncrypt(c.key, plain)
	if cmd == cmdControl {
		payload = append(append([]byte{}, version33Header...), payload...)
	}
	return packFrame(seq, cmd, payload), nil
}

func packFrame(seq, cmd uint32, payload []byte) []byte {
	buf := make([]byte, 16, 16+len(payload)+8)
	binary.BigEndian.PutUint32(buf[0:], framePrefix)
	binary.BigEndian.PutUint32(buf[4:], seq)
	binary.BigEndian.PutUint32(buf[8:], cmd)
	binary.BigEndian.PutUint32(buf[12:], uint32(len(payload)+8))
	buf = append(buf, payload...)
	buf = binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
	return binary.BigEndian.AppendUint32(buf, frameSuffix)
}

// readFrame reads one frame from r and decrypts its payload with key.
func readFrame(r io.Reader, key []byte) (tuyaFrame, error) {
	var hdr [16]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return tuyaFrame{}, err
	}
	if binary.BigEndian.Uint32(hdr[0:]) != framePrefix {
		return tuyaFrame{}, errors.New("bad frame prefix (wrong port or protocol version?)")
	}
	f := tuyaFrame{Seq: binary.BigEndian.Uint32(hdr[4:]), Cmd: binary.BigEndian.Uint32(hdr[8:])}
	n := binary.BigEndian.Uint32(hdr[12:])
	if n < 8 || n > 64<<10 {
		return f, fmt.Errorf("bad frame length %d", n)
	}
	rest := make([]byte, n)
	if _, err := io.ReadFull(r, rest); err != nil {
		return f, err
	}
	body, tail := rest[:n-8], rest[n-8:]
	if binary.BigEndian.Uint32(tail[4:]) != frameSuffix {
		return f, errors.New("bad frame suffix")
	}
	if crc := crc32.ChecksumIEEE(append(hdr[:], body...)); crc != binary.BigEndian.Uint32(tail[:4]) {
		return f, errors.New("frame CRC mismatch")
	}
	payload, retcode, err := decodePayload(body, key)
	f.RetCode, f.Payload = retcode, payload
	return f, err
}

// decodePayload strips the return code and version header and decrypts.
func decodePayload(body, key []byte) ([]byte, uint32, error) {
	var retcode uint32
	// Device frames carry a return code: a small number in the first 4 bytes.
	if len(body) >= 4 && binary.BigEndian.Uint32(body)&0xFFFFFF00 == 0 {
		retcode = binary.BigEndian.Uint32(body)
		body = body[4:]
	}
	if bytes.HasPrefix(body, []byte("3.3")) && len(body) >= len(version33Header) {
		body = body[len(version33Header):]
	}
	if len(body) == 0 {
		return nil, retcode, nil
	}
	// Errors such as "data format error" come back unencrypted
	if body[0] == '{' || len(body)%aes.BlockSize != 0 {
		return body, retcode, nil
	}
	plain, err := ecbDecrypt(key, body)
	if err != nil {
		return nil, retcode, fmt.Errorf("decrypt (wrong local key?): %w", err)
	}
	return plain, retcode, nil
}

func ecbEncrypt(key, plain []byte) []byte {
	block, _ := aes.NewCipher(key) // key length is checked by newTuyaCodec
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Encrypt(out[i:], data[i:i+aes.BlockSize])
	}
	return out
}

func ecbDecrypt(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("ciphertext is not a whole number of blocks")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(out[i:], data[i:i+aes.BlockSize])
	}
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("bad padding")
	}
	return out[:len(out)-pad], nil
}

// parseDPs pulls the data points out of a query reply or status push.
func parseDPs(payload []byte) (map[string]any, error) {
	var msg struct {
		DPS  map[string]any `json:"dps"`
		Data struct {
			DPS map[string]any `json:"dps"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, fmt.Errorf("unexpected reply %q", truncate(string(payload), 120))
	}
	if msg.DPS != nil {
		return msg.DPS, nil
	}
	return msg.Data.DPS, nil
}

// udpKey decrypts the discovery broadcasts protocol 3.3 devices send on UDP 6667.
var udpKey = func() []byte { k := md5.Sum([]byte("yGAdlopoPVldABfn")); return k[:] }()

// DiscoveredDevice is what a device announces about itself on the LAN.
type DiscoveredDevice struct {
	IP         string `json:"ip"`
	ID         string `json:"gwId"`
	ProductKey string `json:"productKey"`
	Version    string `json:"version"`
}

// discover listens for device broadcasts for d and returns what it heard.
func discover(d time.Duration) ([]DiscoveredDevice, error) {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		found = map[string]DiscoveredDevice{}
	)
	for _, port := range []int{6666, 6667} {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err != nil {
			return nil, fmt.Errorf("listen on UDP %d: %w", port, err)
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(d))
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			for {
				n, _, err := conn.ReadFromUDP(buf)
				if err != nil {
					return // deadline reached
				}
				f, err := readFrame(bytes.NewReader(buf[:n]), udpKey)
				if err != nil {
					continue
				}
				var dev DiscoveredDevice
				if json.Unmarshal(f.Payload, &dev) == nil && dev.ID != "" {
					mu.Lock()
					found[dev.ID] = dev
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	out := []DiscoveredDevice{}
	for _, dev := range found {
		out = append(out, dev)
	}
	return out, nil
}
