package smtp

import (
	"strconv"
	"strings"
)

// cutPrefixFold strips a case-insensitive prefix from the original string, so
// the address keeps its case and the offsets stay valid (ToUpper can resize).
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// splitPath returns the address and any trailing ESMTP parameters. The angle
// brackets are optional, since some clients omit them.
func splitPath(arg string) (addr, params string, ok bool) {
	arg = strings.TrimLeft(arg, " \t")

	if strings.HasPrefix(arg, "<") {
		end := strings.Index(arg, ">")
		if end < 0 {
			return "", "", false
		}
		return arg[1:end], strings.TrimSpace(arg[end+1:]), true
	}

	addr, params, _ = strings.Cut(arg, " ")
	if addr == "" {
		return "", "", false
	}
	return addr, strings.TrimSpace(params), true
}

// sizeParam reads SIZE=. When absent or unparseable the cap is enforced while
// reading DATA instead.
func sizeParam(params string) (size int, given bool) {
	for _, p := range strings.Fields(params) {
		value, ok := strings.CutPrefix(strings.ToUpper(p), "SIZE=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
