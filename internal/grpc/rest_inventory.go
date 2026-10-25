// rest_inventory.go exposes the InventoryService's six RPCs as RESTful routes.
//
// Why this file exists: the inventory service was complete at the gRPC layer
// (six RPCs, authorizer judged, policy tests) but was absent from BOTH route
// trees — it is not in grpcServiceHandlers' proto-JSON table and had no
// RESTful resource — so the console could not manage host groups, import an
// inventory file, or see a host's history at all. The CLI reached the same
// features by direct store access in local mode, which is why the gap stayed
// invisible: every command worked, the web just had less than the binary.
//
// Routes (all under /api/v1, matching the targets tree's shape):
//
//	GET    /groups                          → ListGroups
//	POST   /groups                          → CreateGroup
//	DELETE /groups/{id}                     → DeleteGroup
//	POST   /targets/import                  → ImportTargets
//	POST   /targets/{id}/status             → SetTargetStatus
//	GET    /targets/{id}/history?limit=N    → TargetHistory (keyed by the
//	                                          row's HOSTNAME — the proto's
//	                                          TargetHistoryRequest.Host is
//	                                          an address, not an id)
//
// Judgements stay in the service: every handler is a thin translate between
// REST bodies and the proto messages the service already gates. The gateway
// holds the service, not a reimplementation of its policy.
package grpc

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
)

// SetInventory registers the in-process InventoryService with the gateway.
// The gateway needs the concrete type (not the pb server interface) because
// the RESTful handlers call the service's methods directly with translated
// requests, the same way the targets tree calls TargetService.
func (gw *Gateway) SetInventory(inv *InventoryService) { gw.inventory = inv }

// dispatchInventory routes the /groups tree and the inventory-shaped target
// subpaths. It is registered in restResourceHandlers under "groups"; the
// import and history routes hang off the existing "targets" dispatch.
func (gw *Gateway) dispatchInventory(w http.ResponseWriter, r *http.Request, method, path string) {
	inv := gw.inventory
	if inv == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "InventoryService not configured")
		return
	}
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch {
	case path == "/groups" && method == http.MethodGet:
		resp, err := inv.ListGroups(ctx, &pb.ListGroupsRequest{})
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeJSON(w, resp.GetGroups())

	case path == "/groups" && method == http.MethodPost:
		var body struct {
			Name     string `json:"name"`
			ParentID string `json:"parent_id,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			writeJSONError(w, http.StatusBadRequest, "name is required")
			return
		}
		resp, err := inv.CreateGroup(ctx, &pb.CreateGroupRequest{Name: body.Name, ParentId: body.ParentID})
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeJSON(w, resp)

	default:
		// /groups/{id} and /groups/{id}/...
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(path, "/groups"), "/"), "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid groups path")
			return
		}
		id := parts[0]
		if len(parts) == 1 && method == http.MethodDelete {
			resp, err := inv.DeleteGroup(ctx, &pb.DeleteGroupRequest{Id: id})
			if err != nil {
				writeGRPCError(w, err)
				return
			}
			writeJSON(w, resp)
			return
		}
		writeJSONError(w, http.StatusBadRequest, "unknown groups sub-path: /"+path)
	}
}

// handleTargetImport serves POST /targets/import.
func (gw *Gateway) handleTargetImport(w http.ResponseWriter, r *http.Request) {
	inv := gw.inventory
	if inv == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "InventoryService not configured")
		return
	}
	var body struct {
		YamlContent  string `json:"yaml"`
		DefaultGroup string `json:"default_group,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(body.YamlContent) == "" {
		writeJSONError(w, http.StatusBadRequest, "yaml is required")
		return
	}
	resp, err := inv.ImportTargets(metadata.NewOutgoingContext(r.Context(), extractAuth(r)), &pb.ImportTargetsRequest{
		YamlContent:  body.YamlContent,
		DefaultGroup: body.DefaultGroup,
	})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, resp)
}

// handleTargetStatus serves POST /targets/{id}/status.
func (gw *Gateway) handleTargetStatus(w http.ResponseWriter, r *http.Request, id string) {
	inv := gw.inventory
	if inv == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "InventoryService not configured")
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	resp, err := inv.SetTargetStatus(metadata.NewOutgoingContext(r.Context(), extractAuth(r)), &pb.SetTargetStatusRequest{TargetId: id, Status: body.Status})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, resp)
}

// handleTargetHistory serves GET /targets/{id}/history?limit=N. The proto's
// request is keyed by HOST address, so the handler resolves the id to its row
// first — a history keyed by a generated id would be useless to an operator
// reading it next to the target's hostname.
func (gw *Gateway) handleTargetHistory(w http.ResponseWriter, r *http.Request, id string) {
	inv := gw.inventory
	if inv == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "InventoryService not configured")
		return
	}
	row, err := gw.store.GetTarget(r.Context(), id)
	if err != nil {
		writeGRPCError(w, status.Errorf(codes.Internal, "get target: %v", err))
		return
	}
	if row == nil {
		writeJSONError(w, http.StatusNotFound, "target "+id+" not found")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	resp, err := inv.TargetHistory(metadata.NewOutgoingContext(r.Context(), extractAuth(r)), &pb.TargetHistoryRequest{Host: row.Hostname, Limit: int32(limit)})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, resp.GetEntries())
}
