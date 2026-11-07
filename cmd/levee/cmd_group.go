// cmd_group.go — `levee group` sub-commands for the hierarchical target
// inventory: add, list and a tree view of parent-child relations.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// newGroupID generates a random hex id with the grp- prefix. A crypto/rand
// failure is returned as an error — group IDs are uniqueness-critical.
func newGroupID() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate group id: %w", err)
	}
	return "grp-" + hex.EncodeToString(b), nil
}

var (
	groupAddOptParent string
)

func init() {
	RegisterCommand(newGroupCmd())
}

// newGroupCmd builds the `levee group` sub-command with its children.
func newGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group",
		Short: "Manage inventory target groups",
		Long: "Hierarchical groups organize targets (e.g. prod/db). Targets " +
			"belong to at most one group; labels cover cross-cutting dimensions.",
	}

	cmd.AddCommand(newGroupAddCmd())
	cmd.AddCommand(newGroupListCmd())

	return cmd
}

func newGroupAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create an inventory group",
		Long:  "Create a group with a unique path-style name such as prod/db.",
		Args:  cobra.ExactArgs(1),
		RunE:  runGroupAdd,
	}
	cmd.Flags().StringVar(&groupAddOptParent, "parent", "", "Parent group name")
	return cmd
}

func newGroupListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List inventory groups",
		Args:  cobra.NoArgs,
		RunE:  runGroupList,
	}
}

func runGroupAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	parentID := ""
	if groupAddOptParent != "" {
		p, err := store.GetInventoryGroupByName(ctx, groupAddOptParent)
		if err != nil {
			return fmt.Errorf("resolve parent: %w", err)
		}
		if p == nil {
			return fmt.Errorf("parent group %q not found", groupAddOptParent)
		}
		parentID = p.ID
	}

	id, err := newGroupID()
	if err != nil {
		return err
	}
	g := &state.InventoryGroup{ID: id, Name: name, ParentID: parentID}
	if err := store.UpsertInventoryGroup(ctx, g); err != nil {
		return fmt.Errorf("create group: %w", err)
	}

	output := map[string]any{"id": g.ID, "name": g.Name, "parent_id": g.ParentID}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": output, "meta": nil, "error": nil})
	}
	fmt.Fprintf(os.Stdout, "Created group %s (%s)\n", g.Name, g.ID)
	return nil
}

func runGroupList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	mode, err := resolveServiceMode()
	if err != nil {
		return err
	}
	groups, err := fetchGroups(ctx, mode)
	if err != nil {
		return err
	}
	if groups == nil {
		groups = []*state.InventoryGroup{}
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": groups, "meta": nil, "error": nil})
	}

	printGroupListHuman(os.Stdout, groups)
	return nil
}

// fetchGroups returns the group rows in the mode the operator asked for. Both
// modes hand back []*state.InventoryGroup, so the --json and human renderers
// below are shared and cannot drift apart per mode.
func fetchGroups(ctx context.Context, mode serviceMode) ([]*state.InventoryGroup, error) {
	if mode != modeRemote {
		return listGroupsLocal(ctx)
	}
	return listGroupsRemote(ctx)
}

func listGroupsLocal(ctx context.Context) ([]*state.InventoryGroup, error) {
	store, err := openStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	groups, err := store.ListInventoryGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	return groups, nil
}

func listGroupsRemote(ctx context.Context) ([]*state.InventoryGroup, error) {
	f, err := newServiceFactory(modeRemote, optServer, optAPIToken)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	svc, err := f.GetInventoryService()
	if err != nil {
		return nil, err
	}
	resp, err := svc.ListGroups(ctx, &pb.ListGroupsRequest{})
	if err != nil {
		return nil, err
	}

	out := make([]*state.InventoryGroup, 0, len(resp.GetGroups()))
	for _, g := range resp.GetGroups() {
		out = append(out, groupFromPB(g))
	}
	return out, nil
}

// groupFromPB rebuilds the row the local path hands to the same renderer. The
// instant is rebuilt from BOTH wire fields: the column is stamped with
// time.Now() and both dialects keep the fraction, so a seconds-only rebuild
// would differ from the local output on every row rather than only on rows that
// happened to land on a whole second.
func groupFromPB(g *pb.Group) *state.InventoryGroup {
	return &state.InventoryGroup{
		ID:        g.GetId(),
		Name:      g.GetName(),
		ParentID:  g.GetParentId(),
		CreatedAt: time.Unix(g.GetCreatedAt(), int64(g.GetCreatedAtNanos())).UTC(),
		TenantID:  g.GetTenantId(),
	}
}

func printGroupListHuman(w io.Writer, groups []*state.InventoryGroup) {
	if len(groups) == 0 {
		fmt.Fprintln(w, "No groups. Create one with `levee group add <name>`.")
		return
	}
	fmt.Fprintf(w, "%-24s %-22s %s\n", "NAME", "ID", "PARENT_ID")
	for _, g := range groups {
		fmt.Fprintf(w, "%-24s %-22s %s\n", g.Name, g.ID, g.ParentID)
	}
}
