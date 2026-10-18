//go:build !integration

package tagged

import "testing"

func TestTagged(t *testing.T) {
	t.Skip("built without -tags=integration")
}
