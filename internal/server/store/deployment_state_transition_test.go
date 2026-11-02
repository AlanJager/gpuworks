package store

import (
	"errors"
	"reflect"
	"testing"

	"gorm.io/gorm"
)

func TestDeploymentStateTransitionsPreserveDeletion(t *testing.T) {
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	for _, tc := range []struct {
		from, target, want DeploymentState
		err                error
	}{
		{DeployActive, DeployActive, DeployActive, nil},
		{DeployActive, DeployStopped, DeployStopped, nil},
		{DeployActive, DeployDeleting, DeployDeleting, nil},
		{DeployStopped, DeployActive, DeployActive, nil},
		{DeployStopped, DeployStopped, DeployStopped, nil},
		{DeployStopped, DeployDeleting, DeployDeleting, nil},
		{DeployDeleting, DeployActive, DeployDeleting, ErrStateChanged},
		{DeployDeleting, DeployStopped, DeployDeleting, ErrStateChanged},
		{DeployDeleting, DeployDeleting, DeployDeleting, nil},
	} {
		t.Run(string(tc.from)+"_to_"+string(tc.target), func(t *testing.T) {
			dep, err := st.CreateDeployment(&Deployment{Name: t.Name(), State: tc.from})
			if err != nil {
				t.Fatal(err)
			}
			before, err := st.GetDeployment(dep.ID)
			if err != nil {
				t.Fatal(err)
			}
			sub := st.Bus.Subscribe(8, "deployment")
			defer sub.Close()
			if err := st.SetDeploymentState(dep.ID, tc.target); !errors.Is(err, tc.err) {
				t.Errorf("transition error = %v, want %v", err, tc.err)
			}
			after, err := st.GetDeployment(dep.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != tc.want {
				t.Errorf("state = %s, want %s", after.State, tc.want)
			}
			if tc.err != nil {
				if !reflect.DeepEqual(before, after) {
					t.Error("a refused transition changed the deployment")
				}
				select {
				case ev := <-sub.C():
					t.Errorf("a refused transition published %+v", ev)
				default:
				}
			}
		})
	}
}

func TestDeploymentStateMissingDoesNotPublish(t *testing.T) {
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	sub := st.Bus.Subscribe(8, "deployment")
	defer sub.Close()
	for _, target := range DeploymentStates() {
		if err := st.SetDeploymentState(1, target); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing deployment to %s: %v, want ErrNotFound", target, err)
		}
	}
	select {
	case ev := <-sub.C():
		t.Errorf("a missing deployment published %+v", ev)
	default:
	}
}

func TestDeploymentStopChecksDeletionAtWrite(t *testing.T) {
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	dep, err := st.CreateDeployment(&Deployment{State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	name := "test:delete_before_stop_write"
	if err := st.DB().Callback().Update().Before("gorm:begin_transaction").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table != "deployments" || committed {
			return
		}
		// Commit a competing delete immediately before the stop's SQL. A
		// pre-read outside that write must not authorize a stale stop.
		committed = true
		if err := st.SetDeploymentState(dep.ID, DeployDeleting); err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer st.DB().Callback().Update().Remove(name)
	if err := st.SetDeploymentState(dep.ID, DeployStopped); !errors.Is(err, ErrStateChanged) {
		t.Errorf("stop error = %v, want ErrStateChanged", err)
	}
	after, err := st.GetDeployment(dep.ID)
	if !committed || err != nil || after.State != DeployDeleting {
		t.Fatalf("delete committed=%v deployment=%+v err=%v", committed, after, err)
	}
}

func TestDeploymentStateWriteFailureDoesNotPublish(t *testing.T) {
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	dep, err := st.CreateDeployment(&Deployment{State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Exec(`CREATE TRIGGER reject_deployment_state BEFORE UPDATE ON deployments
		BEGIN SELECT RAISE(ABORT, 'injected state write failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(8, "deployment")
	defer sub.Close()
	if err := st.SetDeploymentState(dep.ID, DeployStopped); err == nil || errors.Is(err, ErrStateChanged) || errors.Is(err, ErrNotFound) {
		t.Fatalf("write failure = %v", err)
	}
	after, err := st.GetDeployment(dep.ID)
	if err != nil || after.State != DeployActive {
		t.Fatalf("failed write changed deployment: %+v err=%v", after, err)
	}
	select {
	case ev := <-sub.C():
		t.Errorf("failed write published %+v", ev)
	default:
	}
}
