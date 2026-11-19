//go:build !unix

package fifo

import (
	"context"
	"errors"
)

// Open fails with errors.ErrUnsupported: named pipes are Unix's, and so are the transfers through
// them.
func Open(context.Context, Opener, Way) (*Pipe, error) {
	return nil, errors.ErrUnsupported
}

func (p *Pipe) read(context.Context, []byte) (int, error) {
	return 0, errors.ErrUnsupported
}

func (p *Pipe) write(context.Context, []byte) (int, error) {
	return 0, errors.ErrUnsupported
}
