package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDownloadErrorCodeRequiresKnownTerminalOwnedReport(t *testing.T) {
	for _, tc := range []struct {
		name, state, code, token string
		want                     string
	}{
		{"failure", proto.MFFailed, proto.ErrorModelDownloadSourceNotFound, "current", proto.ErrorModelDownloadSourceNotFound},
		{"retrying", proto.MFDownloading, proto.ErrorModelDownloadNetworkExhausted, "current", ""},
		{"unknown", proto.MFFailed, "FORGED_ERROR", "current", ""},
		{"old transfer", proto.MFFailed, proto.ErrorModelDownloadSourceNotFound, "old", ""},
		{"legacy report", proto.MFFailed, proto.ErrorModelDownloadSourceNotFound, "", ""},
		{"ready", proto.MFPresent, proto.ErrorModelDownloadSourceNotFound, "current", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _, node, ref := downloadTransferFixture(t)
			row, err := st.PrepareModelDownload(ref, "test/download")
			if err != nil {
				t.Fatal(err)
			}
			token := tc.token
			if token == "current" {
				token = row.TransferID
			}
			if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: ModelFileKey(row.ID), State: tc.state, TransferID: token, TransferErrorCode: tc.code, Acquisition: proto.ModelAcquireDownload}); err != nil {
				t.Fatal(err)
			}
			got, err := st.ModelFileByID(row.ID)
			if err != nil || got.TransferErrorCode != tc.want {
				t.Fatalf("row=%+v err=%v want code=%q", got, err, tc.want)
			}
			if tc.token == "old" && got.State != proto.MFPending {
				t.Fatalf("stale failure changed state: %+v", got)
			}
		})
	}
}

func TestDownloadErrorCodeClearsOnRecoveryCancelAndReady(t *testing.T) {
	for _, action := range []string{"retry", "cancel", "ready"} {
		t.Run(action, func(t *testing.T) {
			st, _, node, ref := downloadTransferFixture(t)
			row, err := st.PrepareModelDownload(ref, "test/download")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: ModelFileKey(row.ID), State: proto.MFFailed, TransferID: row.TransferID, TransferErrorCode: proto.ErrorModelDownloadNetworkExhausted}); err != nil {
				t.Fatal(err)
			}
			failed, _ := st.ModelFileByID(row.ID)
			if failed.TransferErrorCode == "" {
				t.Fatal("failure code missing before recovery")
			}
			switch action {
			case "retry":
				_, err = st.PrepareModelDownload(ref, "test/download")
			case "cancel":
				_, err = st.CancelModelDownload(row.ID, row.TransferID, "canceled by user")
			case "ready":
				err = st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: ModelFileKey(row.ID), State: proto.MFPresent, TransferID: row.TransferID})
			}
			got, _ := st.ModelFileByID(row.ID)
			if err != nil || got.TransferErrorCode != "" {
				t.Fatalf("%s retained old code: %+v err=%v", action, got, err)
			}
		})
	}
}
