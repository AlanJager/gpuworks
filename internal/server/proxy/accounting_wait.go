package proxy

// Wait joins the final sink writer after Run returns. Run bounds its own
// slow-sink grace period; an owner releasing the sink must additionally wait
// for this barrier. No subsequent Run may begin until Wait returns.
func (a *Accountant) Wait() {
	if a == nil {
		return
	}
	a.runMu.Lock()
	done := a.writerDone
	a.runMu.Unlock()
	if done != nil {
		<-done
	}
}
