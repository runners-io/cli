// Package sshsync builds the pieces of "rsync over SSH through a ProxyCommand":
// the ssh configuration that names this program as the proxy, and the rsync
// argument list that sends exactly the files the manifest allowed.
//
// It runs nothing. The real transport (which does not exist yet) starts rsync
// with these arguments, writes the manifest's paths to its stdin, and lets ssh
// start `runners scratch proxy --id ID`, which dials the relay.
//
// Two rules are held here and tested:
//
//   - No credential is ever an argument. The proxy finds the token itself, in
//     RUNNERS_TOKEN or the credentials file, so nothing secret is in `ps`, in
//     the ssh configuration, or in shell history.
//   - Nothing the user or the service supplies reaches a shell unquoted. The
//     machine id is restricted to a conservative alphabet (an id starting with
//     "-" would otherwise be an ssh option), and this program's own path is
//     single-quoted for the shell that runs ProxyCommand.
package sshsync

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Workspace is where the box holds the synced tree (design section 6). It is
// deliberately not a look-alike of the Actions workspace.
const Workspace = "/workspace"

// User is the account a box's sshd serves.
const User = "runner"

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidID reports whether id is safe to use as an ssh host name and as a
// ProxyCommand argument.
func ValidID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("%q is not a machine id (lower-case letters, digits and hyphens, starting with a letter or digit)", id)
	}
	return nil
}

// ShellQuote single-quotes s for a POSIX shell.
func ShellQuote(s string) (string, error) {
	if strings.ContainsAny(s, "\n\r\x00") {
		return "", errors.New("a path with a line break or NUL in it cannot be used as the proxy")
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'", nil
}

// Config is the contents of the ssh configuration file, passed to ssh with -F.
// self is the path of this executable. %h is ssh's token for the host name,
// which Args sets to the machine id.
//
// Host-key policy and the client key are not set here: they belong to the
// relay's authentication design (design section 2.3), which is not built.
func Config(self string) (string, error) {
	q, err := ShellQuote(self)
	if err != nil {
		return "", err
	}
	return "Host *\n" +
		"  User " + User + "\n" +
		"  BatchMode yes\n" +
		"  ProxyCommand " + q + " scratch proxy --id %h\n", nil
}

// Options describe one sync.
type Options struct {
	ID         string
	Dir        string // the local working directory
	ConfigPath string // where Config's text was written; no spaces or quotes
}

// Args is the rsync argument list (without the program name). The file list is
// NOT in it: it goes to rsync's stdin as NUL-separated paths
// (--files-from=- --from0), so a path with a newline or a leading dash is just
// a path, and the list is not limited by the length of a command line.
func Args(o Options) ([]string, error) {
	if err := ValidID(o.ID); err != nil {
		return nil, err
	}
	if o.Dir == "" {
		return nil, errors.New("no local directory to send")
	}
	if o.ConfigPath == "" || strings.ContainsAny(o.ConfigPath, " \t\n'\"\\") {
		return nil, fmt.Errorf("the ssh configuration path %q must not contain spaces or quotes (rsync splits -e on them)", o.ConfigPath)
	}
	return append(FlagPrefix(), "-e", "ssh -F "+o.ConfigPath,
		strings.TrimRight(o.Dir, "/")+"/", User+"@"+o.ID+":"+Workspace+"/"), nil
}

// FlagPrefix is the part of Args that does not name a machine or a path:
// recursive, keep times and permissions, read the list from stdin.
func FlagPrefix() []string {
	return []string{"-rtp", "--files-from=-", "--from0"}
}
