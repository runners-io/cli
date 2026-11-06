// Package credentials keeps the personal token `login` obtains.
//
// It lives in one file, mode 0600, in a directory of mode 0700, under
// $XDG_CONFIG_HOME/runners or ~/.config/runners (design section 6). The token
// is a secret: this package never returns it in an error message, and Load
// refuses a file that other users can read, the way ssh refuses a private key
// with loose permissions, because a token that was ever world-readable has to
// be assumed read.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrNone means there is no stored credential.
var ErrNone = errors.New("no stored credential")

// Store is where the CLI keeps its token.
type Store interface {
	// Load returns the token, or ErrNone.
	Load() (string, error)
	Save(token string) error
	// Delete removes the stored token; deleting nothing is not an error.
	Delete() error
}

// DefaultPath is the credentials file for an environment, or "" when neither
// XDG_CONFIG_HOME nor HOME is set.
func DefaultPath(lookup func(string) (string, bool)) string {
	if v, ok := lookup("XDG_CONFIG_HOME"); ok && v != "" {
		return filepath.Join(v, "runners", "credentials")
	}
	if v, ok := lookup("HOME"); ok && v != "" {
		return filepath.Join(v, ".config", "runners", "credentials")
	}
	return ""
}

// File is a Store backed by one file.
type File struct{ Path string }

var _ Store = File{}

type document struct {
	Token string `json:"token"`
}

func (f File) Load() (string, error) {
	if f.Path == "" {
		return "", ErrNone
	}
	info, err := os.Lstat(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNone
	}
	if err != nil {
		return "", fmt.Errorf("reading the credentials file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the credentials file %s is not a regular file", f.Path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("the credentials file %s is readable by other users (mode %04o); run `chmod 600 %s`, or `logout` and sign in again", f.Path, info.Mode().Perm(), f.Path)
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return "", fmt.Errorf("reading the credentials file: %w", err)
	}
	var d document
	if err := json.Unmarshal(b, &d); err != nil || strings.TrimSpace(d.Token) == "" {
		// Do not wrap err: a decoder error can quote the input.
		return "", fmt.Errorf("the credentials file %s is not in the expected format; `logout` and sign in again", f.Path)
	}
	return d.Token, nil
}

// Save writes the token to a new 0600 file and renames it into place, so a
// reader never sees a half-written token and the file is never wider than 0600
// even for an instant.
func (f File) Save(token string) error {
	if f.Path == "" {
		return errors.New("no place to keep the credential: set HOME or XDG_CONFIG_HOME")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("refusing to store an empty token")
	}
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	b, err := json.Marshal(document{Token: token})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*") // CreateTemp makes the file 0600
	if err != nil {
		return fmt.Errorf("writing in %s: %w", dir, err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("writing the credentials file in %s failed", dir)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, f.Path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("replacing %s: %w", f.Path, err)
	}
	return nil
}

func (f File) Delete() error {
	if f.Path == "" {
		return nil
	}
	if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
