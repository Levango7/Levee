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

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/state"
)

// newGroupID generates a random hex id with the grp- prefix.
func newGroupID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "grp-" + hex.EncodeToString(b)
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

	g := &state.InventoryGroup{ID: newGroupID(), Name: name, ParentID: parentID}
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
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	groups, err := store.ListInventoryGroups(ctx)
	if err != nil {
		return fmt.Errorf("list groups: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": groups, "meta": nil, "error": nil})
	}

	printGroupListHuman(os.Stdout, groups)
	return nil
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
