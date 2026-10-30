package api

import (
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// schedulingBoardNode answers GET /scheduling/board?node=<uuid>: the board of
// one node - each GPU with its holders - as a one-row page, and none of the
// fleet-wide lists. It reads that node and its instances only, so opening one
// node of a large fleet costs the same as on a small one.
func (a *apiHandlers) schedulingBoardNode(w http.ResponseWriter, uuid string) {
	node, err := a.d.Store.NodeWithGPUs(uuid)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	nameOf, levelOf := a.deploymentLabels()
	online := node.State == store.NodeStateOnline
	if a.d.Online != nil {
		online = a.d.Online(node.UUID)
	}
	writeJSON(w, http.StatusOK, dto.SchedulingBoard{
		Nodes:   dto.ListPage[dto.BoardNode]{Items: []dto.BoardNode{a.boardNode(node, online, nameOf, levelOf)}, Page: 1, PageSize: 1, Total: 1},
		Pending: []dto.BoardPending{}, WaitingWakes: []dto.BoardWaitingWake{}, Upcoming: []dto.BoardUpcomingAction{}, Recent: []dto.BoardEvent{},
	})
}
