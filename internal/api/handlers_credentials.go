package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/provision"
	"github.com/3th1nk/mammoth/internal/store"
)

// ── credentials ─────────────────────────────────────────────────────────────
// Credentials are write-only: the secret is stored encrypted and never
// returned by any endpoint (docs/04-install-spec.md §2).

func (s *Server) CreateCredential(ctx context.Context, request gen.CreateCredentialRequestObject) (gen.CreateCredentialResponseObject, error) {
	body := request.Body
	if body == nil || body.Name == "" || body.Secret.Username == "" {
		return nil, verr("SCHEMA_INVALID_CREDENTIAL", "name and secret.username are required")
	}
	secret, _ := json.Marshal(body.Secret)
	sealed, err := s.Crypto.Encrypt(secret)
	if err != nil {
		return nil, err
	}
	cred := &store.Credential{
		ID:              store.NewID("cred"),
		Name:            body.Name,
		Type:            string(body.Type),
		SecretEncrypted: sealed,
	}
	if err := s.Credentials.Create(ctx, cred); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "credential", cred.ID, "credential.created", map[string]any{"name": cred.Name})
	obs.FromContext(ctx).InfoContext(ctx, "credential created",
		"credential_id", cred.ID, "type", cred.Type)
	return gen.CreateCredential201JSONResponse(credentialOut(cred)), nil
}

func (s *Server) GetCredential(ctx context.Context, request gen.GetCredentialRequestObject) (gen.GetCredentialResponseObject, error) {
	cred, err := s.Credentials.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetCredential200JSONResponse(credentialOut(cred)), nil
}

func (s *Server) DeleteCredential(ctx context.Context, request gen.DeleteCredentialRequestObject) (gen.DeleteCredentialResponseObject, error) {
	if err := s.Credentials.Delete(ctx, string(request.Id)); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "credential", string(request.Id), "credential.deleted", nil)
	return gen.DeleteCredential204Response{}, nil
}

// ── machines ────────────────────────────────────────────────────────────────

func (s *Server) CreateMachine(ctx context.Context, request gen.CreateMachineRequestObject) (gen.CreateMachineResponseObject, error) {
	m, err := s.registerMachine(ctx, request.Body)
	if err != nil {
		return nil, err
	}
	return gen.CreateMachine201JSONResponse(machineOut(m)), nil
}

// registerMachine is the shared registration path of POST /machines and the
// claim flow: validate the body, create the row, fire auto-discovery, and
// return the fresh machine.
func (s *Server) registerMachine(ctx context.Context, body *gen.MachineCreate) (*store.Machine, error) {
	if body == nil || body.Bmc.Address == "" {
		return nil, verr("SCHEMA_INVALID_MACHINE", "bmc.address is required")
	}
	protocol := "auto"
	if body.Bmc.Protocol != nil && *body.Bmc.Protocol != "" {
		protocol = string(*body.Bmc.Protocol)
	}
	m := &store.Machine{
		ID:              store.NewID("mch"),
		Labels:          derefOr(body.Labels, map[string]string{}),
		BMCAddress:      body.Bmc.Address,
		BMCProtocol:     protocol,
		BMCCredentialID: string(body.Bmc.CredentialId),
		SSHCredentialID: derefStr(body.SshCredentialId),
		SSHAddress:      sshAddressOf(body.Ssh),
	}
	// The referenced credential must exist and be a BMC credential.
	cred, err := s.Credentials.Get(ctx, m.BMCCredentialID)
	if err != nil {
		return nil, verr("SCHEMA_UNKNOWN_CREDENTIAL", "bmc credential %q not found", m.BMCCredentialID)
	}
	if cred.Type != "bmc" {
		return nil, verr("SCHEMA_INVALID_CREDENTIAL", "credential %q is not a bmc credential", m.BMCCredentialID)
	}
	if err := s.Machines.Create(ctx, m); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "machine", m.ID, "machine.created", map[string]any{"bmc_address": m.BMCAddress})
	obs.FromContext(ctx).InfoContext(ctx, "machine registered",
		obs.FieldMachineID, m.ID, obs.FieldBMCAddr, m.BMCAddress, "protocol", m.BMCProtocol)

	// Auto-discovery on registration (docs/05-inventory.md §5): a machine
	// should reach a complete spec view without a second API call. Failure
	// to enqueue leaves the machine in registering — the explicit discover
	// action is the retry path.
	if _, err := s.createJobRecord(ctx, createJobRecord{
		jobType:    "discover",
		flow:       provision.FlowDiscover,
		machineIDs: []string{m.ID},
		actionRaw:  json.RawMessage(`{"type":"discover","probe":"auto"}`),
		createdBy:  "system",
	}); err != nil {
		obs.FromContext(ctx).ErrorContext(ctx, "auto-discovery enqueue failed",
			obs.FieldMachineID, m.ID, "err", err.Error())
	}

	fresh, err := s.Machines.Get(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	return fresh, nil
}

func (s *Server) GetMachine(ctx context.Context, request gen.GetMachineRequestObject) (gen.GetMachineResponseObject, error) {
	m, err := s.Machines.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetMachine200JSONResponse(machineOut(m)), nil
}

func (s *Server) ListMachines(ctx context.Context, request gen.ListMachinesRequestObject) (gen.ListMachinesResponseObject, error) {
	p := request.Params
	orderBy := ""
	if p.OrderBy != nil {
		orderBy = string(*p.OrderBy)
	}
	items, next, err := s.Machines.List(ctx, store.ListFilter{
		State:    string(derefOr(p.State, gen.MachineState(""))),
		Labels:   derefOr(p.Labels, nil),
		Q:        derefOr(p.Q, ""),
		OrderBy:  orderBy,
		PageSize: int(derefOr(p.PageSize, 50)),
		Cursor:   decodeCursor(p.Cursor),
		Order:    orderOf((*string)(p.Order)),
	})
	if err != nil {
		return nil, err
	}
	out := gen.MachineList{Items: []gen.Machine{}}
	for _, m := range items {
		out.Items = append(out.Items, machineOut(m))
	}
	out.NextCursor = encodeCursor(next)
	return gen.ListMachines200JSONResponse(out), nil
}

// ListMachineCurrentTasks is the machine's in-flight view — the console's
// busy badge and reinstall precheck (empty items means free). It reads the
// unfinished tasks straight from the store instead of inferring them from
// the job list, so 409 JOB_MACHINE_BUSY surprises become a pre-check.
func (s *Server) ListMachineCurrentTasks(ctx context.Context, request gen.ListMachineCurrentTasksRequestObject) (gen.ListMachineCurrentTasksResponseObject, error) {
	if _, err := s.Machines.Get(ctx, string(request.Id)); err != nil {
		return nil, err
	}
	refs, err := s.Jobs.ActiveTasksByMachine(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	out := gen.CurrentTaskList{Items: []gen.CurrentTaskRef{}}
	for _, ref := range refs {
		out.Items = append(out.Items, gen.CurrentTaskRef{
			TaskId:    gen.TaskId(ref.ID),
			JobId:     gen.JobId(ref.JobID),
			FlowName:  gen.CurrentTaskRefFlowName(ref.FlowName),
			State:     gen.TaskState(ref.State),
			Attempt:   ref.Attempt,
			CreatedAt: ref.CreatedAt,
		})
	}
	return gen.ListMachineCurrentTasks200JSONResponse(out), nil
}

func (s *Server) UpdateMachine(ctx context.Context, request gen.UpdateMachineRequestObject) (gen.UpdateMachineResponseObject, error) {
	m, err := s.Machines.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	body := request.Body
	if body != nil {
		if body.Labels != nil {
			m.Labels = *body.Labels
		}
		if body.Bmc != nil {
			if body.Bmc.Address != nil {
				m.BMCAddress = *body.Bmc.Address
			}
			if body.Bmc.Protocol != nil {
				m.BMCProtocol = string(*body.Bmc.Protocol)
			}
			if body.Bmc.CredentialId != nil {
				m.BMCCredentialID = string(*body.Bmc.CredentialId)
			}
		}
		if body.SshCredentialId != nil {
			if *body.SshCredentialId == "" {
				m.SSHCredentialID = nil
			} else {
				m.SSHCredentialID = str(string(*body.SshCredentialId))
			}
		}
		if body.Ssh != nil && body.Ssh.Address != nil {
			m.SSHAddress = *body.Ssh.Address
		}
	}
	if err := s.Machines.Update(ctx, m); err != nil {
		return nil, err
	}
	fresh, err := s.Machines.Get(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	return gen.UpdateMachine200JSONResponse(machineOut(fresh)), nil
}

func (s *Server) DeleteMachine(ctx context.Context, request gen.DeleteMachineRequestObject) (gen.DeleteMachineResponseObject, error) {
	if err := s.Machines.Delete(ctx, string(request.Id)); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "machine", string(request.Id), "machine.deleted", nil)
	return gen.DeleteMachine204Response{}, nil
}

// sshAddressOf extracts the in-band address from the optional ssh object.
func sshAddressOf(ssh *gen.MachineSSH) string {
	if ssh == nil || ssh.Address == nil {
		return ""
	}
	return *ssh.Address
}

func (s *Server) GetMachineLayout(ctx context.Context, request gen.GetMachineLayoutRequestObject) (gen.GetMachineLayoutResponseObject, error) {
	if _, err := s.Machines.Get(ctx, string(request.Id)); err != nil {
		return nil, err
	}
	content, captured, err := s.Machines.LatestLayout(ctx, string(request.Id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The contract declares 404 for "no snapshot yet"
			// (docs/03-api.md §2: machines/{id}/layout).
			return nil, verrStatus(http.StatusNotFound, "LAYOUT_SNAPSHOT_REQUIRED",
				"no layout snapshot captured for %s yet", request.Id)
		}
		return nil, err
	}
	var layout gen.Layout
	if err := jsonUnmarshal(content, &layout); err != nil {
		return nil, verr("SCHEMA_INVALID_LAYOUT", "stored layout snapshot is malformed")
	}
	if layout.CapturedAt.IsZero() {
		layout.CapturedAt = captured
	}
	return gen.GetMachineLayout200JSONResponse(layout), nil
}

// GetMachineConsole is the one synchronous BMC touch in the API: a one-time
// KVM URL fetch. Failures surface as 502 problems with BMC_* codes.
func (s *Server) GetMachineConsole(ctx context.Context, request gen.GetMachineConsoleRequestObject) (gen.GetMachineConsoleResponseObject, error) {
	cred, addr, proto, err := s.outOfBandFor(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	res, err := s.BMC.Do(ctx, addr, cred, proto, "console_url", func(ctx context.Context, d bmc.Driver) (any, error) {
		return d.ConsoleURL(ctx, addr, cred)
	})
	if err != nil {
		return nil, err
	}
	url, _ := res.(string)
	return gen.GetMachineConsole200JSONResponse(gen.ConsoleURL{Url: url}), nil
}

// GetMachineBios live-reads the controller's BIOS attribute table
// (docs/07-bmc.md §6). A synchronous BMC read like the console endpoint —
// the machine face cannot hold state that may be stale on the BMC.
func (s *Server) GetMachineBios(ctx context.Context, request gen.GetMachineBiosRequestObject) (gen.GetMachineBiosResponseObject, error) {
	cred, addr, proto, err := s.outOfBandFor(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	res, err := s.BMC.Do(ctx, addr, cred, proto, "bios_attributes", func(ctx context.Context, d bmc.Driver) (any, error) {
		bs, ok := d.(bmc.BiosSetter)
		if !ok {
			return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "bios_attributes"}
		}
		return bs.BiosAttributes(ctx, addr, cred)
	})
	if err != nil {
		return nil, err
	}
	attrs, _ := res.(map[string]any)
	return gen.GetMachineBios200JSONResponse(gen.BiosView{Attributes: attrs}), nil
}

// GetMachineDrives live-reads the controller's physical drive table
// (docs/07-bmc.md §6.2). Serials here are the identity erase_drives
// consumes; a synchronous BMC read like the console/bios endpoints.
func (s *Server) GetMachineDrives(ctx context.Context, request gen.GetMachineDrivesRequestObject) (gen.GetMachineDrivesResponseObject, error) {
	cred, addr, proto, err := s.outOfBandFor(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	res, err := s.BMC.Do(ctx, addr, cred, proto, "physical_drives", func(ctx context.Context, d bmc.Driver) (any, error) {
		pde, ok := d.(bmc.PhysicalDriveEnumerator)
		if !ok {
			return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "physical_drives"}
		}
		return pde.PhysicalDrives(ctx, addr, cred)
	})
	if err != nil {
		return nil, err
	}
	disks, _ := res.([]bmc.DiskView)
	drives := make([]gen.DriveEntry, 0, len(disks))
	for _, disk := range disks {
		item := gen.DriveEntry{Name: disk.Name}
		if disk.Serial != "" {
			item.Serial = &disk.Serial
		}
		if disk.SizeBytes != 0 {
			item.SizeBytes = &disk.SizeBytes
		}
		if disk.Medium != "" {
			item.Medium = &disk.Medium
		}
		if disk.Protocol != "" {
			item.Protocol = &disk.Protocol
		}
		drives = append(drives, item)
	}
	return gen.GetMachineDrives200JSONResponse(gen.DrivesView{Drives: drives}), nil
}

// outOfBandFor resolves machine → (decrypted credentials, address, protocol).
func (s *Server) outOfBandFor(ctx context.Context, machineID string) (bmc.Credentials, string, bmc.Protocol, error) {
	m, err := s.Machines.Get(ctx, machineID)
	if err != nil {
		return bmc.Credentials{}, "", "", err
	}
	credRow, err := s.Credentials.Get(ctx, m.BMCCredentialID)
	if err != nil {
		return bmc.Credentials{}, "", "", verr("SCHEMA_UNKNOWN_CREDENTIAL", "credential %q not found", m.BMCCredentialID)
	}
	plain, err := s.Crypto.Decrypt(credRow.SecretEncrypted)
	if err != nil {
		return bmc.Credentials{}, "", "", verr("CREDENTIAL_DECRYPT_FAILED", "credential decrypt failed")
	}
	var secret struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(plain, &secret); err != nil {
		return bmc.Credentials{}, "", "", verr("CREDENTIAL_DECRYPT_FAILED", "credential payload malformed")
	}
	return bmc.Credentials{Username: secret.Username, Password: secret.Password},
		m.BMCAddress, bmc.Protocol(m.BMCProtocol), nil
}
