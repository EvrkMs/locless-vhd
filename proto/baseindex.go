package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// BaseIndexFile records on the store what a deploy wrote into base.vhdx: the
// version, the table it was laid out with and the hash of every place. The next
// deploy of the same image writes only the places whose hash differs. It is
// removed before the base is written again, so a deploy that stops halfway leaves
// none, and the next one writes the whole image.
const BaseIndexFile = "base.index"

const baseIndexMagic = "LVBI"

// BaseIndex is the content of BaseIndexFile.
type BaseIndex struct {
	Image     string        `json:"image"`
	Version   int           `json:"version"`
	BlockSize uint32        `json:"blockSize"`
	Manifest  Manifest      `json:"manifest"` // as laid out in the virtual disk, after Fit
	Entries   []DeployEntry `json:"-"`
}

// MarshalBinary encodes the index: the magic, the length of the JSON head, the
// head and the entries as in the deploy list.
func (x BaseIndex) MarshalBinary() ([]byte, error) {
	head, err := json.Marshal(x)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 0, 8+len(head)+deployEntrySize*len(x.Entries))
	b = append(b, baseIndexMagic...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(head)))
	b = append(b, head...)
	for _, e := range x.Entries {
		b = AppendDeployEntry(b, e)
	}
	return b, nil
}

// ParseBaseIndex decodes what MarshalBinary made.
func ParseBaseIndex(b []byte) (BaseIndex, error) {
	var x BaseIndex
	if len(b) < 8 || string(b[:4]) != baseIndexMagic {
		return x, errors.New("proto: not a base index")
	}
	n := binary.LittleEndian.Uint32(b[4:])
	if uint64(n) > uint64(len(b)-8) {
		return x, errors.New("proto: truncated base index")
	}
	if err := json.Unmarshal(b[8:8+n], &x); err != nil {
		return x, fmt.Errorf("proto: base index: %w", err)
	}
	entries, err := ParseDeployEntries(b[8+n:])
	if err != nil {
		return x, err
	}
	x.Entries = entries
	return x, nil
}

// SameLayout reports whether two tables, both laid out by Fit for a disk, put
// every partition in the same place with the same identity. Only then may a
// deploy write the blocks of a new version into a disk laid out for an old one.
// The size the clone captured does not count: Fit stretches the volume anyway.
func SameLayout(a, b Manifest) bool {
	if a.DiskSize != b.DiskSize || a.SectorSize != b.SectorSize || !strings.EqualFold(a.DiskGUID, b.DiskGUID) ||
		len(a.Partitions) != len(b.Partitions) {
		return false
	}
	for i, p := range a.Partitions {
		q := b.Partitions[i]
		if p.Number != q.Number || p.Offset != q.Offset || p.Size != q.Size || p.Policy != q.Policy ||
			p.Content != q.Content || p.Attributes != q.Attributes || p.Name != q.Name ||
			!equalGUID(p.TypeGUID, q.TypeGUID) || !equalGUID(p.GUID, q.GUID) {
			return false
		}
	}
	return true
}
