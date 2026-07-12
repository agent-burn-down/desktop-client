package doctor

import "os"

// configPerms captures the config file and directory permission bits, sampled
// before config.Load runs (Load repairs the file mode to 0600 as a side
// effect, which would otherwise mask a bad mode from the check).
type configPerms struct {
	fileKnown bool
	fileMode  os.FileMode
	fileOK    bool
	dir       string
	dirKnown  bool
	dirMode   os.FileMode
	dirOK     bool
}

// statConfigPerms samples the current permission bits of the config file and
// directory. Missing paths leave the corresponding *Known false so the config
// check reports on absence via the load error instead.
func statConfigPerms(path, dir string) configPerms {
	p := configPerms{dir: dir}
	if info, err := os.Stat(path); err == nil {
		p.fileKnown = true
		p.fileMode = info.Mode().Perm()
		p.fileOK = p.fileMode == 0o600
	}
	if info, err := os.Stat(dir); err == nil {
		p.dirKnown = true
		p.dirMode = info.Mode().Perm()
		p.dirOK = p.dirMode == 0o700
	}
	return p
}
