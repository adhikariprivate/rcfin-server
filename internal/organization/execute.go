package organization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/metadata/provider"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

// OrganizeInboxMovies plans arrivals and, when dryRun is false, executes only
// journaled same-library operations supported by the configured backend.
func OrganizeInboxMovies(ctx context.Context, coll *collection.Collection, p provider.Provider, repo database.Repository, opts Options, dryRun bool, worker string) (Report, error) {
	return organizeInbox(ctx, coll, p, repo, opts, dryRun, worker, planReadyInboxMovies)
}

// OrganizeExistingMovies handles only paths outside the configured Inbox. The
// caller must enable this migration mode separately from Inbox organization.
func OrganizeExistingMovies(ctx context.Context, coll *collection.Collection, p provider.Provider, repo database.Repository, opts Options, dryRun bool, worker string) (Report, error) {
	return organizeLibrary(ctx, coll, p, repo, opts, dryRun, worker, planExistingMovies, false)
}

// OrganizeExistingShows normalizes existing TV paths without scanning Inbox.
func OrganizeExistingShows(ctx context.Context, coll *collection.Collection, p provider.Provider, repo database.Repository, opts Options, dryRun bool, worker string) (Report, error) {
	return organizeLibrary(ctx, coll, p, repo, opts, dryRun, worker, planExistingShows, false)
}

// OrganizeInboxShows processes only configured Inbox show trees. It shares the
// same stability, journaling, and recovery controls as movie organization.
func OrganizeInboxShows(ctx context.Context, coll *collection.Collection, p provider.Provider, repo database.Repository, opts Options, dryRun bool, worker string) (Report, error) {
	return organizeInbox(ctx, coll, p, repo, opts, dryRun, worker, planInboxShows)
}

// QueueExistingItemForInbox journals an explicit administrator request to
// stage one existing movie or show directory for the normal Inbox pipeline.
// The move is executed by the organization worker and is recoverable by the
// same source/destination reconciliation used for normalization.
func QueueExistingItemForInbox(ctx context.Context, coll *collection.Collection, repo database.Repository, source string) (*model.MediaOperation, error) {
	if coll == nil || coll.Storage == nil || repo == nil {
		return nil, fmt.Errorf("collection storage and database repository are required")
	}
	if source == "" || source == "." || path.IsAbs(source) {
		return nil, fmt.Errorf("source must be an existing library directory outside the Inbox")
	}
	if strings.Contains(source, "\\") || path.Clean(source) != source || strings.HasPrefix(source, "../") || path.IsAbs(source) {
		return nil, fmt.Errorf("source must be a safe library-relative path")
	}
	action := "stage_existing"
	if existing, lookupErr := repo.GetMediaOperation(ctx, coll.ID, source, action); lookupErr != nil {
		return nil, lookupErr
	} else if existing != nil {
		if isCompleteOperationStatus(existing.Status) || existing.Status == "planned" || existing.Status == "running" {
			return existing, nil
		}
		return nil, fmt.Errorf("previous staging operation is %s; retry it before requesting again", existing.Status)
	}
	capabilities := coll.Storage.Capabilities()
	if !capabilities.DirMove {
		return nil, fmt.Errorf("backend does not advertise DirMove")
	}
	if !capabilities.Local && !capabilities.Copy {
		return nil, fmt.Errorf("remote staging requires advertised Copy for verified directory moves")
	}
	if !capabilities.Put {
		return nil, fmt.Errorf("library VFS is read-only")
	}
	info, err := coll.Storage.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("stat source: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source must be a movie or show directory")
	}
	knownItem := false
	stableItemID := coll.StableItemID(source, nil)
	for _, item := range coll.Items {
		if item != nil && item.Path() == source {
			knownItem = true
			stableItemID = item.ID()
			break
		}
	}
	if !knownItem {
		entries, inventoryErr := collection.InventoryCollection(ctx, coll, coll.InboxPath, false)
		if inventoryErr != nil {
			return nil, fmt.Errorf("inspect source media directory: %w", inventoryErr)
		}
		for _, entry := range entries {
			switch coll.Type {
			case collection.CollectionTypeMovies:
				if entry.Role == collection.InventoryVideo && !entry.IsDir && path.Dir(entry.Path) == source {
					knownItem = true
				} else if entry.Role == collection.InventoryDisc && entry.IsDir && path.Dir(entry.Path) == source && collection.IsMovieDiscStructure(entry, entries) {
					knownItem = true
				}
			case collection.CollectionTypeShows:
				if entry.Role == collection.InventoryVideo && !entry.IsDir && collection.InventoryShowRoot(entry.Path) == source {
					knownItem = true
				}
			}
			if knownItem {
				break
			}
		}
	}
	if !knownItem {
		return nil, fmt.Errorf("source directory is not a recognized movie or show item")
	}
	inbox := strings.Trim(coll.InboxPath, "/")
	if inbox == "" || inbox == "." || path.IsAbs(inbox) || path.Clean(inbox) != inbox || strings.HasPrefix(inbox, "../") {
		return nil, fmt.Errorf("configured Inbox path is invalid")
	}
	if source == inbox || strings.HasPrefix(source, inbox+"/") {
		return nil, fmt.Errorf("source is already inside the Inbox")
	}
	if strings.HasPrefix(inbox, source+"/") {
		return nil, fmt.Errorf("source directory contains the configured Inbox")
	}
	if inboxInfo, statErr := coll.Storage.Stat(inbox); statErr != nil {
		return nil, fmt.Errorf("configured Inbox must already exist: %w", statErr)
	} else if !inboxInfo.IsDir() {
		return nil, fmt.Errorf("configured Inbox path is not a directory")
	}
	destination := path.Join(inbox, path.Base(source))
	if _, statErr := coll.Storage.Stat(destination); statErr == nil {
		return nil, fmt.Errorf("Inbox destination already exists")
	} else if !rclonestore.IsNotFound(statErr) {
		return nil, fmt.Errorf("check Inbox destination: %w", statErr)
	}
	hash := sha256.Sum256([]byte(coll.ID + "\x00" + source + "\x00" + action))
	now := time.Now().UTC()
	evidence, _ := json.Marshal(struct {
		RequestedBy  string `json:"requested_by"`
		StableItemID string `json:"stable_item_id,omitempty"`
	}{"administrator", stableItemID})
	op := &model.MediaOperation{ID: hex.EncodeToString(hash[:]), CollectionID: coll.ID, ItemKey: source, SourcePath: source, DestinationPath: destination, Action: action, Status: "planned", NextAttempt: now, LeaseUntil: now, CreatedAt: now, UpdatedAt: now, Evidence: string(evidence)}
	created, err := repo.CreateMediaOperation(ctx, op)
	if err != nil {
		return nil, err
	}
	if !created {
		existing, lookupErr := repo.GetMediaOperation(ctx, coll.ID, source, action)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if existing == nil {
			return nil, fmt.Errorf("staging operation conflicts with an existing journal record")
		}
		if existing.Status == "planned" || existing.Status == "running" || isCompleteOperationStatus(existing.Status) {
			return existing, nil
		}
		return nil, fmt.Errorf("previous staging operation is %s; review or retry it before requesting again", existing.Status)
	}
	return op, nil
}

// ProcessPendingInboxStaging executes only explicit administrator staging
// requests. It is called independently of automatic organization policy, so
// the requested move can complete even when automatic normalization is off.
func ProcessPendingInboxStaging(ctx context.Context, coll *collection.Collection, repo database.Repository, worker string) (bool, error) {
	if coll == nil || repo == nil {
		return false, fmt.Errorf("collection and database repository are required")
	}
	if worker == "" {
		worker = database.NewMediaOperationWorkerID("organization")
	}
	ctx, leaseKeeper, stopLeaseKeeper := database.StartMediaOperationLeaseKeeper(ctx, repo, worker, 5*time.Minute)
	defer stopLeaseKeeper()
	operations, err := database.ListAllMediaOperations(ctx, repo, coll.ID)
	if err != nil {
		return false, err
	}
	changed := false
	for i := range operations {
		op := &operations[i]
		if op.Action != "stage_existing" || (op.Status != "planned" && op.Status != "retryable" && op.Status != "failed_retryable" && op.Status != "running") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		changed = true // A failed move can still have changed source/destination state.
		if err := runOperation(ctx, coll, repo, worker, op, leaseKeeper); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

type inboxPlanner func(context.Context, *collection.Collection, provider.Provider, Options, map[string]bool) (Report, error)

func organizeInbox(ctx context.Context, coll *collection.Collection, p provider.Provider, repo database.Repository, opts Options, dryRun bool, worker string, planner inboxPlanner) (Report, error) {
	return organizeLibrary(ctx, coll, p, repo, opts, dryRun, worker, planner, true)
}

func organizeLibrary(ctx context.Context, coll *collection.Collection, p provider.Provider, repo database.Repository, opts Options, dryRun bool, worker string, planner inboxPlanner, checkInboxStability bool) (Report, error) {
	if coll == nil || repo == nil {
		return Report{}, fmt.Errorf("collection and database repository are required")
	}
	if worker == "" {
		worker = database.NewMediaOperationWorkerID("organization")
	}
	var leaseKeeper *database.MediaOperationLeaseKeeper
	if !dryRun {
		var stopLeaseKeeper func()
		ctx, leaseKeeper, stopLeaseKeeper = database.StartMediaOperationLeaseKeeper(ctx, repo, worker, 5*time.Minute)
		defer stopLeaseKeeper()
		if err := recoverPending(ctx, coll, repo, worker, leaseKeeper); err != nil {
			return Report{}, err
		}
	}
	// Establish upload stability before making provider calls or producing
	// destination plans. In dry-run mode this remains read-only, so an initial
	// observation is persisted only by an enabled organization run.
	ready := map[string]bool{}
	var staged []ItemPlan
	if checkInboxStability {
		inboxEntries, inventoryErr := collection.InventoryCollection(ctx, coll, coll.InboxPath, true)
		if inventoryErr != nil {
			return Report{}, inventoryErr
		}
		for _, entry := range inboxEntries {
			discRoot := opts.EnableDiscFolders && coll.Type == collection.CollectionTypeMovies && entry.Role == collection.InventoryDisc && entry.IsDir
			if entry.Role != collection.InventoryVideo && !discRoot {
				continue
			}
			source := InboxItemRoot(entry.Path, coll.Type)
			if _, seen := ready[source]; seen {
				continue
			}
			isReady, readyErr := inboxItemReady(ctx, coll, repo, source, opts, !dryRun)
			if readyErr != nil {
				return Report{CollectionID: coll.ID, DryRun: dryRun}, readyErr
			}
			ready[source] = isReady
			if !isReady {
				staged = append(staged, ItemPlan{Source: source, State: "staged", Reason: "Inbox item has not met the completion marker or stable-upload requirement"})
			}
		}
	}
	sort.Slice(staged, func(i, j int) bool { return staged[i].Source < staged[j].Source })
	planned, err := planner(ctx, coll, p, opts, ready)
	if err != nil {
		return planned, err
	}
	report := planned
	report.DryRun = dryRun
	report.Items = append(staged, report.Items...)
	if dryRun {
		// Planning is read-only; no operation records are written.
		return report, nil
	}
	if err := recordOrganizationReviewOperations(ctx, repo, coll.ID, report.Items); err != nil {
		return report, err
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.State != "planned" {
			continue
		}
		for _, file := range item.Files {
			action := journalAction(file)
			source := file.Source
			if strings.HasPrefix(file.Source, item.Source+"/") {
				source = path.Join(item.Destination, strings.TrimPrefix(file.Source, item.Source+"/"))
			}
			op := makeOperation(coll.ID, item.Source, source, file.Destination, action, file.ExpectedBytes, item, opts)
			existing, err := repo.GetMediaOperation(ctx, op.CollectionID, op.ItemKey, op.Action)
			if err != nil {
				return report, err
			}
			if existing != nil {
				// A fresh planner result is the operator-visible recheck after a
				// needs_review/conflict state. Requeue only when the item now passes
				// identity and destination preflight; ambiguous or still-conflicting
				// items never reach this loop as planned operations.
				switch existing.Status {
				case "needs_review", "conflict":
					op.CreatedAt, op.Attempts = existing.CreatedAt, existing.Attempts
				case "planned", "retryable", "failed_retryable", "running", "complete", "completed", "failed_terminal", "failed":
					continue
				default:
					continue
				}
			}
			if err := repo.UpsertMediaOperation(ctx, &op); err != nil {
				return report, err
			}
		}
	}
	if err := recoverPending(ctx, coll, repo, worker, leaseKeeper); err != nil {
		return report, err
	}
	for i := range report.Items {
		if report.Items[i].State == "planned" {
			report.Items[i].State = "submitted"
			report.Items[i].Reason = "journaled operations are pending or complete; refresh the plan to see final paths"
		}
	}
	return report, nil
}

func recordOrganizationReviewOperations(ctx context.Context, repo database.Repository, collectionID string, items []ItemPlan) error {
	const action = "organization_review"
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.Source == "" {
			continue
		}
		existing, err := repo.GetMediaOperation(ctx, collectionID, item.Source, action)
		if err != nil {
			return err
		}
		switch item.State {
		case "needs_review", "conflict":
			identity := collectionID + "\x00" + item.Source + "\x00" + action
			hash := sha256.Sum256([]byte(identity))
			evidence, _ := json.Marshal(struct {
				ConfigVersion int      `json:"config_version"`
				Evidence      []string `json:"match_evidence,omitempty"`
				Item          ItemPlan `json:"item"`
			}{1, item.Evidence, item})
			now := time.Now().UTC()
			op := model.MediaOperation{ID: hex.EncodeToString(hash[:]), CollectionID: collectionID, ItemKey: item.Source, SourcePath: item.Source, DestinationPath: item.Destination, Action: action, Status: item.State, ProviderID: item.ProviderID, Confidence: item.Confidence, Evidence: string(evidence), Error: item.Reason, NextAttempt: now, LeaseUntil: now, CreatedAt: now, UpdatedAt: now}
			if existing != nil {
				op.CreatedAt, op.Attempts = existing.CreatedAt, existing.Attempts
			}
			if err := repo.UpsertMediaOperation(ctx, &op); err != nil {
				return err
			}
		case "planned", "unchanged", "submitted", "complete", "completed":
			if existing == nil || existing.Status != "needs_review" && existing.Status != "conflict" {
				continue
			}
			existing.Status = "complete"
			existing.Error = ""
			existing.UpdatedAt = time.Now().UTC()
			existing.LeaseUntil = existing.UpdatedAt
			if err := repo.UpsertMediaOperation(ctx, existing); err != nil {
				return err
			}
		default:
			continue
		}
	}
	return nil
}

// InboxItemReady records or checks the completion state for one Inbox item.
// Callers that enrich Inbox media must wait for this check before reading it.
func InboxItemReady(ctx context.Context, coll *collection.Collection, repo database.Repository, source string, opts Options) (bool, error) {
	return inboxItemReady(ctx, coll, repo, source, opts, true)
}

// InboxItemRoot returns the item identity used for stability and journaling.
// A TV episode belongs to its containing show directory, one level above a
// recognized season folder.
func InboxItemRoot(entryPath string, kind collection.CollectionType) string {
	dir := path.Dir(entryPath)
	if kind == collection.CollectionTypeShows {
		if _, ok := parseSeasonDirectory(path.Base(dir)); ok {
			return path.Dir(dir)
		}
	}
	return dir
}

// inboxItemReady requires an explicit marker or an unchanged file size and
// modification time over StableFor. The signature and first observation are
// stored in the durable operation journal so restarts do not reset the wait.
func inboxItemReady(ctx context.Context, coll *collection.Collection, repo database.Repository, source string, opts Options, persist bool) (bool, error) {
	if opts.CompletionMarker != "" {
		if path.Base(opts.CompletionMarker) != opts.CompletionMarker || opts.CompletionMarker == "." || opts.CompletionMarker == ".." {
			return false, fmt.Errorf("completion marker must be a filename within the Inbox item")
		}
		_, err := coll.Storage.Stat(path.Join(source, opts.CompletionMarker))
		if err == nil {
			return true, nil
		}
		if rclonestore.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	stableFor := opts.StableFor
	if stableFor <= 0 {
		stableFor = 10 * time.Minute
	}
	entries, err := collection.InventoryCollection(ctx, coll, coll.InboxPath, true)
	if err != nil {
		return false, err
	}
	type itemFile struct {
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		ModTime int64  `json:"mod_time"`
	}
	var files []itemFile
	for _, entry := range entries {
		isMovieFile := entry.Role == collection.InventoryVideo
		isDiscContent := opts.EnableDiscFolders && coll.Type == collection.CollectionTypeMovies && entry.Role == collection.InventoryDisc
		if (!isMovieFile && !isDiscContent) || (path.Dir(entry.Path) != source && !strings.HasPrefix(entry.Path, source+"/")) {
			continue
		}
		info, statErr := coll.Storage.Stat(entry.Path)
		if statErr != nil {
			return false, statErr
		}
		files = append(files, itemFile{Path: entry.Path, Size: info.Size(), ModTime: info.ModTime().UTC().UnixNano()})
	}
	if len(files) == 0 {
		return false, nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	signatureBytes, _ := json.Marshal(files)
	signature := string(signatureBytes)
	const action = "inbox_stability"
	existing, err := repo.GetMediaOperation(ctx, coll.ID, source, action)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	if existing != nil && existing.Evidence == signature {
		return now.Sub(existing.UpdatedAt) >= stableFor, nil
	}
	if !persist {
		return false, nil
	}
	hash := sha256.Sum256([]byte(coll.ID + "\x00" + source + "\x00" + action))
	op := model.MediaOperation{ID: hex.EncodeToString(hash[:]), CollectionID: coll.ID, ItemKey: source, SourcePath: source, Action: action, Status: "stabilizing", Evidence: signature, NextAttempt: now, LeaseUntil: now, CreatedAt: now, UpdatedAt: now}
	if err := repo.UpsertMediaOperation(ctx, &op); err != nil {
		return false, err
	}
	return false, nil
}

func makeOperation(collectionID, itemKey, source, destination, action string, expectedBytes int64, item *ItemPlan, opts Options) model.MediaOperation {
	identity := collectionID + "\x00" + itemKey + "\x00" + action
	hash := sha256.Sum256([]byte(identity))
	evidence, _ := json.Marshal(struct {
		ConfigVersion int      `json:"config_version"`
		MatchEvidence []string `json:"match_evidence"`
		StableItemID  string   `json:"stable_item_id,omitempty"`
		Configuration Options  `json:"configuration"`
	}{1, item.Evidence, item.StableItemID, opts})
	now := time.Now().UTC()
	return model.MediaOperation{ID: hex.EncodeToString(hash[:]), CollectionID: collectionID, ItemKey: itemKey, SourcePath: source, DestinationPath: destination, Action: action, Status: "planned", ProviderID: item.ProviderID, Confidence: item.Confidence, Bytes: expectedBytes, Evidence: string(evidence), NextAttempt: now, CreatedAt: now, UpdatedAt: now}
}

func journalAction(file FilePlan) string {
	if file.Action == "move_file" {
		return file.Action + ":" + file.Source
	}
	return file.Action
}

func recoverPending(ctx context.Context, coll *collection.Collection, repo database.Repository, worker string, leaseKeeper *database.MediaOperationLeaseKeeper) error {
	operations, err := database.ListAllMediaOperations(ctx, repo, coll.ID)
	if err != nil {
		return err
	}
	var pending []model.MediaOperation
	for _, op := range operations {
		if isOrganizationAction(op.Action) && (op.Status == "planned" || op.Status == "retryable" || op.Status == "failed_retryable" || op.Status == "running") {
			pending = append(pending, op)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		if pending[i].ItemKey != pending[j].ItemKey {
			return pending[i].ItemKey < pending[j].ItemKey
		}
		pi, pj := actionPriority(pending[i].Action), actionPriority(pending[j].Action)
		if pi != pj {
			return pi < pj
		}
		return pending[i].Action < pending[j].Action
	})
	for _, op := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		// File renames are relative to the normalized directory. Never attempt
		// them until the directory move has been durably recorded as complete.
		// Otherwise a transient DirMove failure could turn dependent file moves
		// into false "both paths missing" review errors (or rename files in a
		// partially copied destination).
		if strings.HasPrefix(op.Action, "move_directory:") && op.Action != "move_directory:root" {
			rootMove, lookupErr := repo.GetMediaOperation(ctx, coll.ID, op.ItemKey, "move_directory:root")
			if lookupErr != nil {
				return lookupErr
			}
			if rootMove != nil && !isCompleteOperationStatus(rootMove.Status) {
				continue
			}
		}
		if strings.HasPrefix(op.Action, "move_file:") {
			all, lookupErr := database.ListAllMediaOperations(ctx, repo, coll.ID)
			if lookupErr != nil {
				return lookupErr
			}
			readyForFiles := true
			for _, dependency := range all {
				if dependency.ItemKey == op.ItemKey && (dependency.Action == "move_directory" || strings.HasPrefix(dependency.Action, "move_directory:")) && !isCompleteOperationStatus(dependency.Status) {
					readyForFiles = false
					break
				}
			}
			if !readyForFiles {
				continue
			}
		}
		if err := runOperation(ctx, coll, repo, worker, &op, leaseKeeper); err != nil { // Keep processing independent items; their journal records remain visible.
			continue
		}
	}
	return nil
}

func isCompleteOperationStatus(status string) bool {
	// Accept the older organization value while records are gradually rewritten
	// with the plan's canonical "complete" status.
	return status == "complete" || status == "completed"
}

func isOrganizationAction(action string) bool {
	return action == "stage_existing" || action == "create_directory" || action == "move_directory" || strings.HasPrefix(action, "move_directory:") || strings.HasPrefix(action, "move_file:")
}

func actionPriority(action string) int {
	switch {
	case action == "stage_existing":
		return 0
	case action == "create_directory":
		return 0
	case action == "move_directory":
		return 1
	case action == "move_directory:root":
		return 1
	case strings.HasPrefix(action, "move_directory:"):
		return 2
	case strings.HasPrefix(action, "move_file:"):
		return 3
	default:
		return 3
	}
}

func runOperation(ctx context.Context, coll *collection.Collection, repo database.Repository, worker string, op *model.MediaOperation, leaseKeeper *database.MediaOperationLeaseKeeper) error {
	now := time.Now().UTC()
	claimed, err := repo.ClaimMediaOperation(ctx, op.ID, worker, now, now.Add(5*time.Minute))
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	leaseKeeper.Track(op.ID)
	defer leaseKeeper.Done(op.ID)
	op.Status = "running"
	op.LeaseOwner = worker
	op.LeaseUntil = now.Add(5 * time.Minute)
	op.Attempts++
	op.UpdatedAt = now
	if err := repo.UpsertMediaOperation(ctx, op); err != nil {
		return err
	}
	var operationErr error
	switch {
	case op.Action == "create_directory":
		if info, statErr := coll.Storage.Stat(op.DestinationPath); statErr == nil {
			if !info.IsDir() {
				operationErr = fmt.Errorf("directory destination is occupied by a file")
			}
		} else if rclonestore.IsNotFound(statErr) {
			operationErr = coll.Storage.EnsureDirectory(op.DestinationPath)
		} else {
			operationErr = statErr
		}
	case op.Action == "stage_existing" || op.Action == "move_directory" || strings.HasPrefix(op.Action, "move_directory:") || strings.HasPrefix(op.Action, "move_file:"):
		sourceExists, sourceInfo, sourceErr := statPath(coll, op.SourcePath)
		destinationExists, destinationInfo, destinationErr := statPath(coll, op.DestinationPath)
		if sourceErr != nil && !rclonestore.IsNotFound(sourceErr) {
			operationErr = sourceErr
		} else if destinationErr != nil && !rclonestore.IsNotFound(destinationErr) {
			operationErr = destinationErr
		} else if !sourceExists && destinationExists {
			directoryOperation := !strings.HasPrefix(op.Action, "move_file:")
			if directoryOperation && !destinationInfo.IsDir() {
				operationErr = fmt.Errorf("recovered directory destination is not a directory; operation needs review")
				return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
			}
			if directoryOperation {
				destinationBytes, sizeErr := coll.Storage.DirectorySize(ctx, op.DestinationPath)
				if sizeErr != nil {
					status, next := operationFailureState(sizeErr, op.Attempts, now)
					return finishOperation(ctx, repo, op, status, sizeErr, next)
				}
				if destinationBytes != op.Bytes {
					operationErr = fmt.Errorf("recovered directory destination size differs from journaled source size; operation needs review")
					return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
				}
			}
			if !directoryOperation && destinationInfo.IsDir() {
				operationErr = fmt.Errorf("recovered file destination is a directory; operation needs review")
				return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
			}
			if strings.HasPrefix(op.Action, "move_file:") && destinationInfo.Size() != op.Bytes {
				operationErr = fmt.Errorf("recovered file destination size %d differs from journaled source size %d; operation needs review", destinationInfo.Size(), op.Bytes)
				return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
			}
			if strings.HasPrefix(op.Action, "move_file:") && op.Attempts > 1 {
				if sourceInfo.Size() != op.Bytes || destinationInfo.Size() != op.Bytes || op.Checksum == "" {
					operationErr = fmt.Errorf("source and destination files do not match the journaled move evidence; operation needs review")
					return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
				}
				sourceChecksum, sourceChecksumErr := coll.Storage.ContentChecksum(ctx, op.SourcePath)
				destinationChecksum, destinationChecksumErr := coll.Storage.ContentChecksum(ctx, op.DestinationPath)
				if sourceChecksumErr != nil || destinationChecksumErr != nil {
					checkErr := sourceChecksumErr
					if checkErr == nil {
						checkErr = destinationChecksumErr
					}
					status, next := operationFailureState(checkErr, op.Attempts, now)
					return finishOperation(ctx, repo, op, status, checkErr, next)
				}
				if sourceChecksum != op.Checksum || destinationChecksum != op.Checksum {
					operationErr = fmt.Errorf("source and destination file checksums do not match the journaled move evidence; operation needs review")
					return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
				}
			}
			if op.Checksum != "" {
				destinationChecksum, checksumErr := coll.Storage.ContentChecksum(ctx, op.DestinationPath)
				if checksumErr != nil {
					status, next := operationFailureState(checksumErr, op.Attempts, now)
					return finishOperation(ctx, repo, op, status, checksumErr, next)
				}
				if destinationChecksum == "" || destinationChecksum != op.Checksum {
					operationErr = fmt.Errorf("recovered destination checksum does not match journaled source checksum; operation needs review")
					return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
				}
			}
			return finishOperation(ctx, repo, op, "complete", nil, time.Time{})
		} else if sourceExists && destinationExists {
			directoryOperation := !strings.HasPrefix(op.Action, "move_file:")
			if directoryOperation && op.Attempts > 1 {
				if !sourceInfo.IsDir() || !destinationInfo.IsDir() {
					operationErr = fmt.Errorf("source and destination types do not match the journaled directory move; operation needs review")
					return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
				}
				sourceBytes, sourceSizeErr := coll.Storage.DirectorySize(ctx, op.SourcePath)
				destinationBytes, destinationSizeErr := coll.Storage.DirectorySize(ctx, op.DestinationPath)
				if sourceSizeErr != nil || destinationSizeErr != nil {
					checkErr := sourceSizeErr
					if checkErr == nil {
						checkErr = destinationSizeErr
					}
					status, next := operationFailureState(checkErr, op.Attempts, now)
					return finishOperation(ctx, repo, op, status, checkErr, next)
				}
				if sourceBytes != op.Bytes || destinationBytes != op.Bytes || op.Checksum == "" {
					operationErr = fmt.Errorf("source and destination directories do not match the journaled move evidence; operation needs review")
					return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
				}
				sourceChecksum, sourceChecksumErr := coll.Storage.ContentChecksum(ctx, op.SourcePath)
				destinationChecksum, destinationChecksumErr := coll.Storage.ContentChecksum(ctx, op.DestinationPath)
				if sourceChecksumErr != nil || destinationChecksumErr != nil {
					checkErr := sourceChecksumErr
					if checkErr == nil {
						checkErr = destinationChecksumErr
					}
					status, next := operationFailureState(checkErr, op.Attempts, now)
					return finishOperation(ctx, repo, op, status, checkErr, next)
				}
				if sourceChecksum != op.Checksum || destinationChecksum != op.Checksum {
					operationErr = fmt.Errorf("source and destination directory checksums do not match the journaled move evidence; operation needs review")
					return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
				}
				operationErr = coll.Storage.MoveDirectory(ctx, op.SourcePath, op.DestinationPath, true)
			} else if strings.HasPrefix(op.Action, "move_file:") && op.Attempts > 1 {
				operationErr = coll.Storage.MoveFile(ctx, op.SourcePath, op.DestinationPath, true)
			} else {
				operationErr = fmt.Errorf("source and destination both exist; operation needs review")
			}
		} else if !sourceExists && !destinationExists {
			operationErr = fmt.Errorf("source and destination are both missing; operation needs review")
		} else {
			directoryOperation := !strings.HasPrefix(op.Action, "move_file:")
			if directoryOperation != sourceInfo.IsDir() {
				operationErr = fmt.Errorf("source type does not match the journaled directory or file operation; operation needs review")
				return finishOperation(ctx, repo, op, "needs_review", operationErr, time.Time{})
			}
			if directoryOperation {
				op.Bytes, operationErr = coll.Storage.DirectorySize(ctx, op.SourcePath)
				if operationErr != nil {
					status, next := operationFailureState(operationErr, op.Attempts, now)
					return finishOperation(ctx, repo, op, status, operationErr, next)
				}
			} else {
				op.Bytes = sourceInfo.Size()
			}
			op.Checksum, operationErr = coll.Storage.ContentChecksum(ctx, op.SourcePath)
			if operationErr != nil {
				status, next := operationFailureState(operationErr, op.Attempts, now)
				return finishOperation(ctx, repo, op, status, operationErr, next)
			}
			op.UpdatedAt = time.Now().UTC()
			if operationErr = repo.UpsertMediaOperation(ctx, op); operationErr != nil {
				return operationErr
			}
			if op.Action == "stage_existing" || op.Action == "move_directory" || strings.HasPrefix(op.Action, "move_directory:") {
				operationErr = coll.Storage.MoveDirectory(ctx, op.SourcePath, op.DestinationPath, op.Attempts > 1)
			} else {
				operationErr = coll.Storage.MoveFile(ctx, op.SourcePath, op.DestinationPath, false)
			}
		}
	default:
		operationErr = fmt.Errorf("unsupported journal action %q", op.Action)
	}
	if operationErr == nil {
		return finishOperation(ctx, repo, op, "complete", nil, time.Time{})
	}
	status, next := operationFailureState(operationErr, op.Attempts, now)
	return finishOperation(ctx, repo, op, status, operationErr, next)
}

func statPath(coll *collection.Collection, name string) (bool, os.FileInfo, error) {
	info, err := coll.Storage.Stat(name)
	if err != nil {
		return false, nil, err
	}
	return true, info, nil
}

func finishOperation(ctx context.Context, repo database.Repository, op *model.MediaOperation, status string, operationErr error, now time.Time) error {
	finished := time.Now().UTC()
	op.Status = status
	op.LeaseUntil = finished
	op.UpdatedAt = finished
	if isCompleteOperationStatus(status) {
		op.Error = ""
		op.NextAttempt = finished
	} else if operationErr != nil {
		op.Error = sanitizeError(operationErr)
		if status == "retryable" || status == "failed_retryable" {
			op.NextAttempt = now
		}
	}
	return repo.UpsertMediaOperation(ctx, op)
}

func operationFailureState(err error, attempt int, now time.Time) (string, time.Time) {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "both exist") || strings.Contains(message, "both missing") || strings.Contains(message, "already exists") || strings.Contains(message, "destination") && strings.Contains(message, "exists") || strings.Contains(message, "checksum differs") || strings.Contains(message, "sizes differ") || strings.Contains(message, "size verification failed") || strings.Contains(message, "does not verify") || strings.Contains(message, "failed verification") || strings.Contains(message, "verification unavailable") {
		return "needs_review", now
	}
	if strings.Contains(message, "permission") || strings.Contains(message, "forbidden") || strings.Contains(message, "unauthorized") || strings.Contains(message, "does not advertise") || strings.Contains(message, "read-only") || strings.Contains(message, "unsupported") {
		return "failed_terminal", now
	}
	if attempt >= 8 {
		return "failed_terminal", now
	}
	delay := time.Duration(1<<min(attempt, 10)) * time.Minute
	if delay > 6*time.Hour {
		delay = 6 * time.Hour
	}
	jitter := time.Duration(rand.Int63n(int64(max(delay/4, 1))))
	return "failed_retryable", now.Add(delay + jitter)
}

var urlPattern = regexp.MustCompile(`https?://[^\s"']+`)

func sanitizeError(err error) string {
	text := strings.Map(func(r rune) rune {
		if r < 32 && r != '\t' {
			return ' '
		}
		return r
	}, err.Error())
	text = urlPattern.ReplaceAllString(text, "[redacted URL]")
	if len(text) > 2048 {
		text = text[:2048]
	}
	return text
}
