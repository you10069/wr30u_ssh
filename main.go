package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
)

const (
	headerLen     = 8
	globalFlag    = uint32(0x3F721FB5)
	globalMAC     = "123456789ABC"
	maxPacketSize = 16 << 20 // Defensive limit: 16 MiB.
)

type elinkHeader struct {
	Flag uint32
	Len  uint32
}

type elinkPacket struct {
	Head elinkHeader
	Data []byte
}

type elinkSession struct {
	conn net.Conn

	sendMu  sync.Mutex
	sendSeq uint64
	recvSeq uint64

	key     []byte
	peerMAC string
	devInfo any

	registered     chan struct{}
	registeredOnce sync.Once
	done           chan struct{}
}

type requestEnvelope struct {
	Type     string          `json:"type"`
	Sequence uint64          `json:"sequence"`
	MAC      string          `json:"mac"`
	Data     json.RawMessage `json:"data"`
}

type dhData struct {
	DHG   string `json:"dh_g"`
	DHP   string `json:"dh_p"`
	DHKey string `json:"dh_key"`
}

func newSession(conn net.Conn) *elinkSession {
	return &elinkSession{
		conn:       conn,
		registered: make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// sendJSON serializes one response while holding the send lock. The sequence
// placed in JSON matches the Python implementation: it is the number of
// packets sent before this packet.
func (s *elinkSession) sendJSON(encrypted bool, build func(sequence uint64) any) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	payload, err := json.Marshal(build(s.sendSeq))
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	if encrypted {
		if len(s.key) == 0 {
			return errors.New("cannot encrypt response before DH key exchange")
		}
		payload, err = encryptData(payload, s.key)
		if err != nil {
			return err
		}
	}

	header := make([]byte, headerLen)
	binary.BigEndian.PutUint32(header[0:4], globalFlag)
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))

	if err := writeAll(s.conn, header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if err := writeAll(s.conn, payload); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}

	s.sendSeq++
	return nil
}

func (s *elinkSession) sendACK(recvSeq uint64) error {
	s.recvSeq = recvSeq
	return s.sendJSON(true, func(_ uint64) any {
		return map[string]any{
			"type":     "ack",
			"sequence": s.recvSeq,
			"mac":      globalMAC,
		}
	})
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[n:]
	}
	return nil
}

func parseHeader(buf []byte) (elinkHeader, error) {
	if len(buf) != headerLen {
		return elinkHeader{}, fmt.Errorf("invalid header length: got %d, need %d", len(buf), headerLen)
	}
	flagValue := binary.BigEndian.Uint32(buf[0:4])
	length := binary.BigEndian.Uint32(buf[4:8])
	if flagValue != globalFlag {
		return elinkHeader{}, fmt.Errorf("header flag mismatch: got 0x%08X, need 0x%08X", flagValue, globalFlag)
	}
	if length > maxPacketSize {
		return elinkHeader{}, fmt.Errorf("packet too large: %d bytes", length)
	}
	return elinkHeader{Flag: flagValue, Len: length}, nil
}

func decryptData(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("invalid AES-CBC ciphertext length: %d", len(ciphertext))
	}

	plaintext := make([]byte, len(ciphertext))
	iv := make([]byte, aes.BlockSize)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
	return bytes.Trim(plaintext, "\x00"), nil
}

func encryptData(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	padLen := (aes.BlockSize - len(plaintext)%aes.BlockSize) % aes.BlockSize
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)

	ciphertext := make([]byte, len(padded))
	iv := make([]byte, aes.BlockSize)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	return ciphertext, nil
}

func handleKeyNGReq(s *elinkSession, req requestEnvelope) error {
	s.peerMAC = req.MAC
	return s.sendJSON(false, func(sequence uint64) any {
		return map[string]any{
			"type":     "keyngack",
			"sequence": sequence,
			"mac":      globalMAC,
			"keymode":  "dh",
		}
	})
}

func handleDH(s *elinkSession, req requestEnvelope) error {
	var data dhData
	if err := json.Unmarshal(req.Data, &data); err != nil {
		return fmt.Errorf("decode DH data: %w", err)
	}

	gBytes, err := base64.StdEncoding.DecodeString(data.DHG)
	if err != nil {
		return fmt.Errorf("decode dh_g: %w", err)
	}
	pBytes, err := base64.StdEncoding.DecodeString(data.DHP)
	if err != nil {
		return fmt.Errorf("decode dh_p: %w", err)
	}
	aliceKeyBytes, err := base64.StdEncoding.DecodeString(data.DHKey)
	if err != nil {
		return fmt.Errorf("decode dh_key: %w", err)
	}
	if len(aliceKeyBytes) == 0 {
		return errors.New("empty DH public key")
	}

	g := new(big.Int).SetBytes(gBytes)
	p := new(big.Int).SetBytes(pBytes)
	alicePublicKey := new(big.Int).SetBytes(aliceKeyBytes)
	if p.Cmp(big.NewInt(5)) < 0 {
		return errors.New("invalid DH prime")
	}

	// Python random.randint(2, p-2), inclusive.
	rangeSize := new(big.Int).Sub(p, big.NewInt(3)) // p-3 possible values.
	randomValue, err := cryptorand.Int(cryptorand.Reader, rangeSize)
	if err != nil {
		return fmt.Errorf("generate DH private key: %w", err)
	}
	bobPrivateKey := new(big.Int).Add(randomValue, big.NewInt(2))
	bobPublicKey := new(big.Int).Exp(g, bobPrivateKey, p)
	sharedKey := new(big.Int).Exp(alicePublicKey, bobPrivateKey, p)

	bobPublicBytes, err := fixedWidthBytes(bobPublicKey, len(aliceKeyBytes))
	if err != nil {
		return fmt.Errorf("encode DH public key: %w", err)
	}
	sharedKeyBytes, err := fixedWidthBytes(sharedKey, len(aliceKeyBytes))
	if err != nil {
		return fmt.Errorf("encode DH shared key: %w", err)
	}
	if _, err := aes.NewCipher(sharedKeyBytes); err != nil {
		return fmt.Errorf("DH produced unsupported AES key length %d: %w", len(sharedKeyBytes), err)
	}
	s.key = sharedKeyBytes

	publicKeyBase64 := base64.StdEncoding.EncodeToString(bobPublicBytes)
	return s.sendJSON(false, func(sequence uint64) any {
		return map[string]any{
			"type":     "dh",
			"sequence": sequence,
			"mac":      globalMAC,
			"data": map[string]string{
				"dh_key": publicKeyBase64,
				"dh_p":   data.DHP,
				"dh_g":   data.DHG,
			},
		}
	})
}

func fixedWidthBytes(value *big.Int, width int) ([]byte, error) {
	raw := value.Bytes()
	if len(raw) > width {
		return nil, fmt.Errorf("integer needs %d bytes, field width is %d", len(raw), width)
	}
	out := make([]byte, width)
	copy(out[width-len(raw):], raw)
	return out, nil
}

func upgradeConfig(s *elinkSession, cfg map[string]any) error {
	return s.sendJSON(true, func(sequence uint64) any {
		return map[string]any{
			"type":     "cfg",
			"sequence": sequence,
			"mac":      globalMAC,
			"set":      cfg,
		}
	})
}

func execute(s *elinkSession, command string) error {
	cfg := map[string]any{
		"upgrade": map[string]string{
			"downurl":  "-h; " + command + " ;echo",
			"isreboot": "0",
		},
	}
	return upgradeConfig(s, cfg)
}

func handlePacket(s *elinkSession, packet elinkPacket) error {
	payload := packet.Data
	var err error
	if len(s.key) != 0 {
		payload, err = decryptData(payload, s.key)
		if err != nil {
			return err
		}
	}

	var req requestEnvelope
	if err := json.Unmarshal(payload, &req); err != nil {
		return fmt.Errorf("decode request JSON: %w", err)
	}

	switch req.Type {
	case "keyngreq":
		log.Printf("recv keyngreq")
		return handleKeyNGReq(s, req)
	case "keyngack":
		log.Printf("recv keyngack")
	case "dh":
		log.Printf("recv dh")
		return handleDH(s, req)
	case "dev_reg":
		log.Printf("recv dev_reg")
		var devInfo any
		if len(req.Data) != 0 {
			if err := json.Unmarshal(req.Data, &devInfo); err != nil {
				return fmt.Errorf("decode device info: %w", err)
			}
		}
		s.devInfo = devInfo
		if err := s.sendACK(req.Sequence); err != nil {
			return err
		}
		s.registeredOnce.Do(func() { close(s.registered) })
	case "keepalive":
		log.Printf("recv keepalive")
		return s.sendACK(req.Sequence)
	case "ack", "cfg", "get_status", "status", "real_devinfo":
		log.Printf("recv %s", req.Type)
	default:
		log.Printf("unknown request type: %q", req.Type)
	}
	return nil
}

func handleConnection(s *elinkSession) {
	defer close(s.done)
	defer func() {
		log.Printf("close connection: %s", s.conn.RemoteAddr())
		_ = s.conn.Close()
	}()

	for {
		headerBuf := make([]byte, headerLen)
		if _, err := io.ReadFull(s.conn, headerBuf); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("read header: %v", err)
			}
			return
		}

		header, err := parseHeader(headerBuf)
		if err != nil {
			log.Printf("parse header: %v", err)
			return
		}

		data := make([]byte, int(header.Len))
		if _, err := io.ReadFull(s.conn, data); err != nil {
			log.Printf("read packet body: %v", err)
			return
		}

		if err := handlePacket(s, elinkPacket{Head: header, Data: data}); err != nil {
			log.Printf("handle packet: %v", err)
			return
		}
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runCommands(s *elinkSession, password string) error {
	quoted := shellQuote(password)
	commands := []string{
		"printf '%s\\n%s\\n' " + quoted + " " + quoted + " | passwd root",
		"nvram set ssh_en=1 && nvram commit",
		`sed -i 's/channel=.*/channel="debug"/g' /etc/init.d/dropbear && /etc/init.d/dropbear start`,
	}
	for _, command := range commands {
		if err := execute(s, command); err != nil {
			return fmt.Errorf("send command %q: %w", command, err)
		}
	}
	return nil
}

func prettyJSON(value any) string {
	buf, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(buf)
}

func main() {
	listenAddr := flag.String("listen", ":32768", "TCP listen address")
	password := flag.String("password", "admin", "new root password")
	auto := flag.Bool("auto", false, "run commands immediately after device registration")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen on %s: %v", *listenAddr, err)
	}
	defer listener.Close()

	fmt.Printf("Waiting for device on %s\n", *listenAddr)
	reader := bufio.NewReader(os.Stdin)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept connection: %v", err)
			continue
		}

		log.Printf("accepted connection from %s", conn.RemoteAddr())
		session := newSession(conn)
		go handleConnection(session)

		select {
		case <-session.registered:
		case <-session.done:
			log.Printf("device disconnected before registration")
			continue
		}

		fmt.Println("Device information:")
		fmt.Println(prettyJSON(session.devInfo))

		if !*auto {
			fmt.Print("Press Enter to enable SSH...")
			_, _ = reader.ReadString('\n')
		}

		if err := runCommands(session, *password); err != nil {
			log.Printf("enable SSH failed: %v", err)
			continue
		}
		fmt.Printf("Finished. SSH account: root / %s\n", *password)
	}
}
