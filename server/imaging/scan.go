package imaging

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"locless-vhd/proto"
	"locless-vhd/server/store"
)

// versionFile is the name the server gives version files: v1.ldisk, v2.ldelta.
var versionFile = regexp.MustCompile(`(?i)^v([0-9]{1,6})\.(ldisk|ldelta)$`)

// Found is the chain ReadFolder found in a folder of an image.
type Found struct {
	Versions []store.Version // oldest first, ready for AddVersion
	Skipped  []string        // version files left out, with the reason
}

// ReadFolder finds the version files in dir and the chain they make: the newest
// .ldisk and the .ldelta files after it, each proved by the files themselves to
// continue the one before. Older files and those that break the chain are left
// out and named in Skipped. A folder without an .ldisk has no chain.
func ReadFolder(dir string) (Found, error) {
	var found Found
	entries, err := os.ReadDir(dir)
	if err != nil {
		return found, err
	}
	type candidate struct {
		number int
		name   string
		full   bool // .ldisk
	}
	var files []candidate
	for _, e := range entries {
		m := versionFile.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		files = append(files, candidate{number: n, name: e.Name(), full: strings.EqualFold(m[2], "ldisk")})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].number < files[j].number })
	start := -1
	for i, f := range files {
		if f.full && f.number > 0 {
			start = i
		}
	}
	if start < 0 {
		for _, f := range files {
			found.Skipped = append(found.Skipped, f.name+": нет полной версии .ldisk")
		}
		return found, nil
	}
	for _, f := range files[:start] {
		found.Skipped = append(found.Skipped, f.name+": старше "+files[start].name)
	}

	var opened []*os.File
	defer func() {
		for _, f := range opened {
			f.Close()
		}
	}()
	var chain []*proto.File
	previous := 0
	for i, f := range files[start:] {
		if i > 0 && (f.full || f.number == previous) {
			found.Skipped = append(found.Skipped, f.name+": не продолжает цепочку")
			continue
		}
		pf, size, err := openVersion(filepath.Join(dir, f.name))
		if pf != nil {
			opened = append(opened, pf.file)
		}
		if err == nil {
			_, err = proto.NewChain(append(chain, pf.image)...)
		}
		if err != nil {
			found.Skipped = append(found.Skipped, fmt.Sprintf("%s: %v", f.name, err))
			if i == 0 {
				return found, nil // the base itself is unreadable: no chain at all
			}
			continue
		}
		chain = append(chain, pf.image)
		found.Versions = append(found.Versions, store.Version{
			Number: f.number, File: f.name, Size: size, Parent: previous,
		})
		previous = f.number
	}
	return found, nil
}

type versionHandle struct {
	file  *os.File
	image *proto.File
}

func openVersion(path string) (*versionHandle, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return &versionHandle{file: f}, 0, err
	}
	pf, err := proto.OpenFile(f, info.Size())
	if err != nil {
		return &versionHandle{file: f}, 0, err
	}
	return &versionHandle{file: f, image: pf}, info.Size(), nil
}
