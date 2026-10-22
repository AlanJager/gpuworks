package conn

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
)

// imageRevision captures the publication order before runtime inspection.
// Inventory collection never holds writeMu: a slow inspection must not delay
// operation completion, cancellation, or other control messages.
func (c *Conn) imageRevision() uint64 {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.imageStatusRevision
}

func (c *Conn) sendNodeStatus(status proto.NodeStatus, imageRevision uint64) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.imageStatusRevision != imageRevision {
		// An explicit operation result was published after collection began. Its
		// image state is newer than this snapshot. Keep all other node facts; the
		// next inventory can report fresh image presence, including real absence.
		status.Images = nil
	}
	return c.sendLocked(proto.TStatusNode, 0, status)
}

func (c *Conn) send(t string, id uint64, d any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.sendLocked(t, id, d); err != nil {
		return err
	}
	if t == proto.TStatusImageNode {
		c.imageStatusRevision++
	}
	return nil
}

// sendLocked is called with writeMu held so freshness checks and publication
// are atomic with respect to image operation results.
func (c *Conn) sendLocked(t string, id uint64, d any) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	line, err := json.Marshal(proto.Envelope{T: t, ID: id, D: raw})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = c.ctrl.Write(line)
	return err
}
