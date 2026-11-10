package connector

import (
	"testing"
)

// TestShotJobStateValues は、撮影ジョブの状態の文字列が、HTTP の応答に出る従来の値のままであることを確かめる。
func TestShotJobStateValues(t *testing.T) {
	if ShotJobPending != "pending" || ShotJobDone != "done" || ShotJobFailed != "failed" {
		t.Errorf("撮影ジョブの状態の値が変わった: %q %q %q", ShotJobPending, ShotJobDone, ShotJobFailed)
	}
}
