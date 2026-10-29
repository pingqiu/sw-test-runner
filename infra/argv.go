package infra

import (
	"context"
	"errors"
	"strings"
)

// ShellArgv preserves every positional argument across the SSH shell boundary,
// including empty arguments. It does not interpret any caller-provided shell.
func ShellArgv(argv ...string) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("empty executable")
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return "", errors.New("NUL in argv")
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " "), nil
}

func (n *Node) RunArgv(ctx context.Context, argv ...string) (string, string, int, error) {
	cmd, err := ShellArgv(argv...)
	if err != nil {
		return "", "", -1, err
	}
	return n.Run(ctx, cmd)
}
