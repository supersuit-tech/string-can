package canon

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxTextBytes is the maximum accepted message text size.
	MaxTextBytes = 65536
	// MaxBodyBytes caps an HTTP POST body, covering worst-case JSON escaping
	// of a MaxTextBytes payload plus file metadata and the canonical envelope.
	// File bytes are not part of this body.
	MaxBodyBytes = MaxTextBytes*6 + 64*1024
	// MaxFiles is the maximum number of files on one message.
	MaxFiles = 8
	// ProtocolMaxFileBytes is the largest size a signed file entry may claim.
	// STRING_CAN_MAX_FILE_BYTES may set a lower limit.
	ProtocolMaxFileBytes int64 = 1 << 30
	keyLen                     = 64
	shaLen                     = 64
)

// File is signed metadata for one attachment. The bytes live outside the
// message, addressed by SHA256.
type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
}

// Message is the signed canonical payload.
// Files is omitted when empty, so a text-only message keeps the v=1 bytes
// older peers already verify.
type Message struct {
	V     int    `json:"v"`
	From  string `json:"from"`
	Ts    int64  `json:"ts"`
	Text  string `json:"text"`
	Files []File `json:"files,omitempty"`
}

// Encode returns the canonical compact JSON bytes for a text-only v=1 message.
func Encode(from string, ts int64, text string) ([]byte, error) {
	return EncodeMessage(from, ts, text, nil)
}

// EncodeMessage returns the canonical compact JSON bytes for a v=1 message.
// files is omitted from the JSON when empty. text may be empty only when
// files is not.
func EncodeMessage(from string, ts int64, text string, files []File) ([]byte, error) {
	from, err := NormalizeKey(from)
	if err != nil {
		return nil, err
	}
	files, err = normalizeFiles(files)
	if err != nil {
		return nil, err
	}
	if err := checkPayload(text, files); err != nil {
		return nil, err
	}
	payload := Message{V: 1, From: from, Ts: ts, Text: text, Files: files}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// Sign returns an unpadded base64url Ed25519 signature of body.
func Sign(priv ed25519.PrivateKey, body []byte) string {
	sig := ed25519.Sign(priv, body)
	return base64.RawURLEncoding.EncodeToString(sig)
}

// Verify checks that sigb64 is a valid unpadded base64url Ed25519 signature
// of body by the given 64-hex public key.
func Verify(pubHex string, body []byte, sigb64 string) error {
	pubHex, err := NormalizeKey(pubHex)
	if err != nil {
		return err
	}
	pub, err := hex.DecodeString(pubHex)
	if err != nil {
		return err
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid public key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigb64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid signature")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), body, sig) {
		return fmt.Errorf("invalid signature")
	}
	return nil
}

// ParseAndCheck unmarshals a canonical body and validates protocol fields.
func ParseAndCheck(body []byte, pathKey string) (Message, error) {
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return Message{}, fmt.Errorf("malformed body")
	}
	pathKey, err := NormalizeKey(pathKey)
	if err != nil {
		return Message{}, err
	}
	if m.V != 1 {
		return Message{}, fmt.Errorf("unsupported version")
	}
	from, err := NormalizeKey(m.From)
	if err != nil || from != pathKey {
		return Message{}, fmt.Errorf("from does not match path")
	}
	m.From = from
	if m.Ts <= 0 {
		return Message{}, fmt.Errorf("invalid timestamp")
	}
	files, err := normalizeFiles(m.Files)
	if err != nil {
		return Message{}, err
	}
	m.Files = files
	if err := checkPayload(m.Text, m.Files); err != nil {
		return Message{}, err
	}
	return m, nil
}

// FilesIn returns the signed file list from a canonical body.
// A text-only body returns nil, nil.
func FilesIn(body string) ([]File, error) {
	var m Message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil, fmt.Errorf("malformed body")
	}
	if len(m.Files) == 0 {
		return nil, nil
	}
	return normalizeFiles(m.Files)
}

// CheckText reports whether text is non-empty valid UTF-8 within the size cap.
func CheckText(text string) error {
	if text == "" {
		return fmt.Errorf("text is empty")
	}
	return checkText(text)
}

func checkPayload(text string, files []File) error {
	if text == "" {
		if len(files) == 0 {
			return fmt.Errorf("text is empty")
		}
		return nil
	}
	return checkText(text)
}

func checkText(text string) error {
	if len(text) > MaxTextBytes {
		return fmt.Errorf("text is too large")
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("text is not valid utf-8")
	}
	return nil
}

// NormalizeKey lowercases a 64-character hex public key.
func NormalizeKey(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != keyLen {
		return "", fmt.Errorf("public key must be 64 hex characters")
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("public key must be 64 hex characters")
	}
	return s, nil
}

// PublicHex returns the 64-character lowercase hex encoding of an Ed25519 public key.
func PublicHex(pub ed25519.PublicKey) string {
	return hex.EncodeToString(pub)
}

// PublicHexFromPrivate returns the 64-character lowercase hex public key.
func PublicHexFromPrivate(priv ed25519.PrivateKey) string {
	return PublicHex(priv.Public().(ed25519.PublicKey))
}

// ShortKey returns the first 8 hex characters of a public key.
func ShortKey(pubHex string) string {
	if len(pubHex) < 8 {
		return pubHex
	}
	return pubHex[:8]
}
