package base

import (
	"testing"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

func TestStateFileReferenceFormat(t *testing.T) {
	const record = 37
	want, _ := ident.New(ident.State, record)
	for _, ntfs := range []bool{false, true} {
		l := locator{ntfs: ntfs}
		if id, ok := l.stateID(record); !ok || id != want {
			t.Fatalf("NTFS=%t: index %d gave %v, %v", ntfs, record, id, ok)
		}
		id, ok := l.stateID(3<<48 | record)
		if ntfs && (!ok || id != want) || !ntfs && (ok || id != 0) {
			t.Fatalf("NTFS=%t: wide file reference gave %v, %v", ntfs, id, ok)
		}
	}
}
