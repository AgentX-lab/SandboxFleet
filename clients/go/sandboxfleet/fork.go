package sandboxfleet

import (
	"context"
	"errors"
	"fmt"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SnapshotOptions creates a SandboxSnapshot from a Running parent.
type SnapshotOptions struct {
	Namespace     string
	Name          string // optional; empty = parent-snap-<uid8>
	SourceSandbox string
	Pool          string
}

// ForkOptions creates one snapshot then N children from it.
type ForkOptions struct {
	ParentNamespace string
	ParentName      string
	Count           int
	// ChildNames optional; if empty uses parent-fork-0..N-1
	ChildNames []string
	// SnapshotName optional
	SnapshotName string
	// SlotProfile optional; defaults to parent
	SlotProfile string
}

type ForkResult struct {
	Snapshot *sandboxv1alpha1.SandboxSnapshot
	Children []*sandboxv1alpha1.Sandbox
}

func (c *sdkClient) CreateSnapshot(ctx context.Context, opts SnapshotOptions) (*sandboxv1alpha1.SandboxSnapshot, error) {
	if opts.Namespace == "" || opts.SourceSandbox == "" || opts.Pool == "" {
		return nil, errors.New("namespace, sourceSandbox, and pool are required")
	}
	name := opts.Name
	if name == "" {
		parent, err := c.GetSandbox(ctx, opts.Namespace, opts.SourceSandbox)
		if err != nil {
			return nil, err
		}
		uid := string(parent.UID)
		if len(uid) > 8 {
			uid = uid[:8]
		}
		name = opts.SourceSandbox + "-snap-" + uid
	}
	snap := &sandboxv1alpha1.SandboxSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: sandboxv1alpha1.GroupVersion.String(),
			Kind:       "SandboxSnapshot",
		},
		ObjectMeta: metav1.ObjectMeta{Namespace: opts.Namespace, Name: name},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{
			SourceSandbox: opts.SourceSandbox,
			Pool:          opts.Pool,
		},
	}
	if err := c.kubernetes.Create(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func (c *sdkClient) GetSnapshot(ctx context.Context, namespace, name string) (*sandboxv1alpha1.SandboxSnapshot, error) {
	var snap sandboxv1alpha1.SandboxSnapshot
	if err := c.kubernetes.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (c *sdkClient) WaitSnapshotReady(ctx context.Context, namespace, name string) (*sandboxv1alpha1.SandboxSnapshot, error) {
	for {
		snap, err := c.GetSnapshot(ctx, namespace, name)
		if err != nil {
			return nil, err
		}
		if snap.Status.Phase == sandboxv1alpha1.SandboxSnapshotPhaseFailed {
			return nil, fmt.Errorf("SandboxSnapshot %s/%s failed: %s", namespace, name, snap.Status.Message)
		}
		ready := meta.FindStatusCondition(snap.Status.Conditions, sandboxv1alpha1.ConditionReady)
		if snap.Status.Phase == sandboxv1alpha1.SandboxSnapshotPhaseReady &&
			ready != nil && ready.Status == metav1.ConditionTrue {
			return snap, nil
		}
		if err := wait(ctx, c.pollInterval); err != nil {
			return nil, err
		}
	}
}

func (c *sdkClient) DeleteSnapshot(ctx context.Context, namespace, name string) error {
	snap, err := c.GetSnapshot(ctx, namespace, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.kubernetes.Delete(ctx, snap)
}

func (c *sdkClient) WaitSnapshotDeleted(ctx context.Context, namespace, name string) error {
	for {
		_, err := c.GetSnapshot(ctx, namespace, name)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := wait(ctx, c.pollInterval); err != nil {
			return err
		}
	}
}

// CreateSandboxFromSnapshot creates a child that restores from a Ready snapshot.
func (c *sdkClient) CreateSandboxFromSnapshot(ctx context.Context, opts CreateOptions, snapshotName string) (*sandboxv1alpha1.Sandbox, error) {
	if opts.Namespace == "" || opts.Name == "" || opts.PoolRef == "" || opts.SlotProfile == "" || snapshotName == "" {
		return nil, errors.New("namespace, name, poolRef, slotProfile, and snapshotName are required")
	}
	sandbox := &sandboxv1alpha1.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: sandboxv1alpha1.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{Namespace: opts.Namespace, Name: opts.Name},
		Spec: sandboxv1alpha1.SandboxSpec{
			PoolRef:      opts.PoolRef,
			SlotProfile:  opts.SlotProfile,
			FromSnapshot: snapshotName,
		},
	}
	if err := c.kubernetes.Create(ctx, sandbox); err != nil {
		return nil, err
	}
	return sandbox, nil
}

// Fork = CreateSnapshot → wait Ready → create N children with fromSnapshot.
func (c *sdkClient) Fork(ctx context.Context, opts ForkOptions) (*ForkResult, error) {
	if opts.ParentNamespace == "" || opts.ParentName == "" || opts.Count < 1 {
		return nil, errors.New("parentNamespace, parentName, and count>=1 are required")
	}
	parent, err := c.WaitSandboxReady(ctx, opts.ParentNamespace, opts.ParentName)
	if err != nil {
		return nil, err
	}
	slotProfile := opts.SlotProfile
	if slotProfile == "" {
		slotProfile = parent.Spec.SlotProfile
	}

	snap, err := c.CreateSnapshot(ctx, SnapshotOptions{
		Namespace:     opts.ParentNamespace,
		Name:          opts.SnapshotName,
		SourceSandbox: opts.ParentName,
		Pool:          parent.Spec.PoolRef,
	})
	if err != nil {
		return nil, err
	}
	snap, err = c.WaitSnapshotReady(ctx, snap.Namespace, snap.Name)
	if err != nil {
		return nil, err
	}

	names := opts.ChildNames
	if len(names) == 0 {
		names = make([]string, opts.Count)
		for i := 0; i < opts.Count; i++ {
			names[i] = fmt.Sprintf("%s-fork-%d", opts.ParentName, i)
		}
	}
	if len(names) != opts.Count {
		return nil, errors.New("childNames length must equal count")
	}

	children := make([]*sandboxv1alpha1.Sandbox, 0, opts.Count)
	for _, name := range names {
		child, err := c.CreateSandboxFromSnapshot(ctx, CreateOptions{
			Namespace:   opts.ParentNamespace,
			Name:        name,
			PoolRef:     parent.Spec.PoolRef,
			SlotProfile: slotProfile,
		}, snap.Name)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return &ForkResult{Snapshot: snap, Children: children}, nil
}
