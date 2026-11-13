package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/files"
	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/store"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("--file path is empty")
	}
	*s = append(*s, v)
	return nil
}

type attachmentJSON struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
	Path   string `json:"path,omitempty"`
}

type threadLine struct {
	ID          int64            `json:"id"`
	From        string           `json:"from"`
	Ts          int64            `json:"ts"`
	Text        string           `json:"text"`
	Attachments []attachmentJSON `json:"attachments,omitempty"`
}

func (e env) threadLine(dir home.Dir, m store.Message) threadLine {
	return threadLine{
		ID:          m.ID,
		From:        m.FromPub,
		Ts:          m.Ts,
		Text:        m.Text,
		Attachments: attachmentViews(dir, m),
	}
}

func attachmentViews(dir home.Dir, m store.Message) []attachmentJSON {
	list, err := canon.FilesIn(m.Body)
	if err != nil || len(list) == 0 {
		return nil
	}
	out := make([]attachmentJSON, 0, len(list))
	for _, f := range list {
		item := attachmentJSON{Name: f.Name, Size: f.Size, Type: f.Type, SHA256: f.SHA256}
		if path, err := files.SavedPath(dir.Path, m.PeerPub, m.ID, f.Name); err == nil {
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				item.Path = path
			}
		}
		out = append(out, item)
	}
	return out
}

func (e env) cmdDownload(ctx context.Context, args []string) int {
	var outDir string
	fs := newFlagSet("download")
	fs.StringVar(&outDir, "out", "", "directory to write; default is the home files folder")
	pos, help, err := parseWith(fs, args, map[string]bool{"out": true})
	if help {
		fmt.Fprint(e.stdout, commandHelp("download"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	if len(pos) != 2 {
		return e.fail(fmt.Errorf("usage: string-can download <public-key> <message-id> [--out <dir>]"))
	}
	key, err := canon.NormalizeKey(pos[0])
	if err != nil {
		return e.fail(err)
	}
	id, err := strconv.ParseInt(pos[1], 10, 64)
	if err != nil || id <= 0 {
		return e.fail(fmt.Errorf("invalid message id"))
	}
	st, dir, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	if _, err := st.GetContact(key); err != nil {
		return e.fail(err)
	}
	priv, err := dir.Identity()
	if err != nil {
		return e.fail(err)
	}
	ourKey := canon.PublicHexFromPrivate(priv)
	m, ok, err := st.MessageByID(key, id)
	if err != nil {
		return e.fail(err)
	}
	if !ok {
		return e.fail(fmt.Errorf("message %d is not in this thread", id))
	}
	if m.FromPub != key && m.FromPub != ourKey {
		return e.fail(fmt.Errorf("refusing files that are not from a contact"))
	}
	if err := canon.Verify(m.FromPub, []byte(m.Body), m.Signature); err != nil {
		return e.fail(err)
	}
	list, err := canon.FilesIn(m.Body)
	if err != nil {
		return e.fail(err)
	}
	if len(list) == 0 {
		return e.fail(fmt.Errorf("message %d has no files", id))
	}
	c, err := st.GetContact(key)
	if err != nil {
		return e.fail(err)
	}
	blobs, err := files.Open(dir.Path)
	if err != nil {
		return e.fail(err)
	}
	if err := e.ensureBlobs(ctx, blobs, c, ourKey, list); err != nil {
		return e.fail(err)
	}
	saved := make([]attachmentJSON, 0, len(list))
	for _, f := range list {
		dest, err := files.SavedPath(dir.Path, key, id, f.Name)
		if err != nil {
			return e.fail(err)
		}
		if err := blobs.Materialize(f.Name, f.SHA256, f.Size, dest); err != nil {
			return e.fail(err)
		}
		shown := dest
		if outDir != "" {
			name, err := canon.SafeName(f.Name)
			if err != nil {
				return e.fail(err)
			}
			if err := os.MkdirAll(outDir, 0o700); err != nil {
				return e.fail(err)
			}
			copyTo := joinOut(outDir, name)
			if err := copyFile(dest, copyTo); err != nil {
				return e.fail(err)
			}
			shown = copyTo
		}
		saved = append(saved, attachmentJSON{
			Name: f.Name, Size: f.Size, Type: f.Type, SHA256: f.SHA256, Path: shown,
		})
	}
	return e.writeJSON(struct {
		ID          int64            `json:"id"`
		Peer        string           `json:"peer"`
		Attachments []attachmentJSON `json:"attachments"`
	}{ID: id, Peer: key, Attachments: saved})
}

// joinOut joins a sanitized name under dir. SafeName already rejected separators.
func joinOut(dir, name string) string {
	return filepath.Join(dir, name)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dest + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Chmod(0o600); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// ensureBlobs fetches any signed blob that is not already stored.
// Bytes are kept only after the SHA-256 matches the signed message.
func (e env) ensureBlobs(ctx context.Context, blobs *files.Store, c store.Contact, ourKey string, list []canon.File) error {
	var missing []canon.File
	for _, f := range list {
		if !blobs.Has(f.SHA256) {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if c.BaseURL == "" {
		return fmt.Errorf("%s is not stored and the contact has no URL", missing[0].Name)
	}
	for _, f := range missing {
		body, err := push.GetBlob(ctx, push.PeerClient(), c.BaseURL, ourKey, f.SHA256)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		err = blobs.SaveVerified(f.Name, f.SHA256, f.Size, body)
		body.Close()
		if err != nil {
			var mismatch *files.HashMismatch
			if errors.As(err, &mismatch) {
				return err
			}
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	return nil
}
