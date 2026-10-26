// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/search"
	"agw/internal/store"
)

// Errors of the training search that the API turns into an answer.
var (
	// ErrSearchNotRunning: the search is not running (any more); the user's controls and the agent's calls need it.
	ErrSearchNotRunning = errors.New("the training search is not running")
)

// SearchInputError is a refused input of the user's controls (HTTP 400).
type SearchInputError struct{ Msg string }

func (e *SearchInputError) Error() string { return e.Msg }

func searchErr(msg string) (platform.Result, error) {
	return platform.Result{Status: "error", Message: msg}, nil
}

// searchEnabled says whether this gateway runs the training search service (registered as the doer of the paths
// under platform.SearchPrefix).
func (m *Manager) searchEnabled() bool {
	_, ok := m.virtualFor(platform.SearchPrefix)
	return ok
}

// SearchMaxHours is the longest a search runs (GET /api/config tells the approval card).
func (m *Manager) SearchMaxHours() int { return m.searchMaxHours() }

// searchMaxHours is the longest a search runs.
func (m *Manager) searchMaxHours() int {
	if m.opt.SearchMaxHours > 0 {
		return m.opt.SearchMaxHours
	}
	return 12
}

// searchFileOf binds one uploaded file: the SHA-256 is computed here, and the text is kept when it can be shown.
func searchFileOf(u platform.Upload) store.SearchFile {
	f := store.SearchFile{Name: u.Name, SHA256: sha256Hex(u.Data), Size: int64(len(u.Data))}
	if strings.HasSuffix(strings.ToLower(u.Name), ".ipynb") {
		if sum, ok := platform.NotebookCodeSHA256(u.Data); ok {
			// The diff of a notebook shows its code cells, not the JSON.
			f.CodeSHA256 = sum
			f.Text, _ = platform.NotebookCodeText(u.Data)
			return f
		}
	}
	if utf8.Valid(u.Data) && len(u.Data) <= platform.NotebookTextMax {
		f.Text = string(u.Data)
	}
	return f
}

// sha256Hex is the SHA-256 of bytes the gateway read itself, never taken from a field.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ProposeSearch takes a proposal of a training search (POST /_agw/search, issue #168): one approval of a search
// space and a budget, inside which the agent then runs trial after trial. The pattern is ProposePlan. Everything
// the user approves is written by the gateway from what it read: the template's settings, the files' SHA-256, the
// test set the split held back.
func (m *Manager) ProposeSearch(ctx context.Context, chatID, slotID, via string, req platform.Request) (platform.Result, error) {
	if !m.searchEnabled() || m.opt.Platform == nil {
		return platform.Result{Status: platform.StatusUnavailable, Message: platform.UnavailableText(
			"training search", "this gateway has no search service yet", "Run single trainings instead")}, nil
	}
	if m.inTrash(ctx, chatID) {
		return platform.Result{Status: "error", Message: platform.RefusedPrefix + trashRefusal}, nil
	}
	if m.experimentOf(ctx, chatID) != nil {
		return searchErr(platform.RefusedPrefix + "a training search is not available in an experiment chat")
	}
	p, err := search.ParseProposal(req.Form, platform.NotebookTimeoutMax)
	if err != nil {
		return searchErr(err.Error())
	}
	if running, err := m.st.RunningSearch(ctx, chatID); err == nil {
		return searchErr(fmt.Sprintf("training search %s is still running in this chat; end it first ({{tool:end_search}})", running.ID))
	} else if !errors.Is(err, store.ErrNotFound) {
		return platform.Result{}, err
	}
	left := m.computeLeft(ctx, chatID)
	need := left
	if p.Budget.MaxRuns != nil {
		need = *p.Budget.MaxRuns
	}
	if need < 1 || need > left {
		return searchErr(fmt.Sprintf("not enough compute calls left: %d runs need %d, this chat has %d left", max(need, 1), max(need, 1), left))
	}
	rl := &readLog{}
	read := m.loggedSummaryReader(chatID, rl)
	if p.Path == search.PathNotebook {
		read = cachedReads(read)
	}
	data := SearchApprovalData{Kind: searchApprovalProposal, Title: platform.VisibleControls(p.Title), Objective: p.Objective,
		Space: p.Space, Budget: p.Budget, Path: p.Path, Note: platform.VisibleControls(p.Note)}
	data.Compute.Needed, data.Compute.Left, data.Compute.LeftAfter = need, left, left-need
	data.ExpiresAt = planNow().Add(m.opt.ApprovalTimeout)
	data.Autonomy.MaxAutoTurns = maxAutoTurns(need)

	var base json.RawMessage
	var code store.SearchCode
	var holdoutTrain int64
	switch p.Path {
	case search.PathTemplate:
		b, d, msg, err := m.readTemplateBase(ctx, chatID, read, p)
		if err != nil {
			return platform.Result{}, err
		}
		if msg != "" {
			return searchErr(msg)
		}
		base, data.Base, data.Fixed = b.raw, &b.base, b.fixed
		holdoutTrain = d
	case search.PathNotebook:
		c, codeData, ds, msg := m.readNotebookCode(ctx, chatID, read, p, req)
		if msg != "" {
			return searchErr(msg)
		}
		code, data.Code = c, codeData
		holdoutTrain = ds
	}
	data.Holdout = m.holdoutOf(ctx, chatID, read, holdoutTrain)

	if err := m.checkPendingLimit(ctx, chatID); err != nil {
		return platform.Result{}, err
	}
	searchID := ""
	{
		var b [6]byte
		u := uuid.New()
		copy(b[:], u[:6])
		searchID = "s" + hex.EncodeToString(b[:])
	}
	data.SearchID = searchID
	summary := searchSummary(data)
	if n := len([]rune(summary)); n > maxPlatformPreview {
		return searchErr(fmt.Sprintf("search too large for an approval (%d characters, at most %d); shrink the space", n, maxPlatformPreview))
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return platform.Result{}, err
	}
	session := store.SessionFrom(ctx)
	apID := uuid.NewString()
	cur := &searchRecheckState{data: data, base: base}
	defer m.registerRecheck(apID, m.searchRecheck(chatID, apID, p, cur))()
	d := &planDecision{apply: func(bg context.Context, final string) (bool, error) {
		if final != store.ApprovalApproved {
			state := store.SearchRejected
			if final == store.ApprovalExpired {
				state = store.SearchExpired
			}
			if _, _, err := m.st.DecideSearch(bg, searchID, state, nil); err != nil {
				slog.Error("search decision not recorded", "search", searchID, "error", err)
			}
			m.publishSearch(bg, chatID, searchID)
			return false, nil
		}
		ends := planNow().Add(time.Duration(m.searchMaxHours()) * time.Hour)
		if se, ok := m.planSessionEnd(bg, chatID); ok && se.Before(ends) {
			ends = se
		}
		if b := cur.baseNow(); len(b) > 0 {
			if err := m.st.SetSearchBase(bg, searchID, b); err != nil {
				return false, err
			}
		}
		_, ok, err := m.st.DecideSearch(bg, searchID, store.SearchRunning, &ends)
		if err != nil || !ok {
			return ok, err
		}
		m.addSearchEvent(bg, searchID, "approved", map[string]any{"ends_by": ends})
		m.publishSearch(bg, chatID, searchID)
		return true, nil
	}}
	defer m.registerPlanDecision(apID, d)()
	m.logSummaryReads(ctx, chatID, apID, "proposed", rl)
	holdout, _ := json.Marshal(data.Holdout)
	objective, _ := json.Marshal(p.Objective)
	space, _ := json.Marshal(p.Space)
	if _, err := m.st.CreateSearch(ctx, store.Search{ID: searchID, ChatID: chatID, Title: data.Title, Path: p.Path, Objective: objective,
		Space: space, Budget: search.MarshalBudget(p.Budget), Base: base, Code: code, Holdout: holdout, Note: data.Note, Session: session,
		ApprovalID: apID, ToolCallID: store.ToolCallFrom(ctx)}); err != nil {
		return platform.Result{}, err
	}
	ap, err := m.st.CreateApproval(ctx, store.Approval{ID: apID, ChatID: chatID, Kind: "search", Via: via, Name: "Training search: " + data.Title,
		Size: int64(need), ContentType: "application/json", PendingKey: "-", Preview: summary, Summary: summary, Session: session,
		ToolCallID: store.ToolCallFrom(ctx), Search: raw})
	if err != nil {
		return platform.Result{}, err
	}
	w := m.broker.Register(ap.ID)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("waiting_approval", "")
	}
	m.publish(chatID, Event{Kind: "approval", Data: ap})
	m.publishSearch(ctx, chatID, searchID)
	m.publishChat(ctx, chatID)
	slog.Info("search proposed", "chat", chatID, "approval", ap.ID, "search", searchID, "path", p.Path, "via", via)
	approved, werr := w.Wait(ctx, m.opt.ApprovalTimeout)
	bg := context.WithoutCancel(ctx)
	final := m.settle(bg, chatID, ap.ID, approved, werr)
	if s, ok := m.pool.Get(slotID); ok {
		s.SetActivity("tool", "")
	}
	m.publishChat(bg, chatID)
	ok, err := d.decide(bg, final)
	if final != store.ApprovalApproved {
		msg := "search not approved by the user; nothing executed. Ask the user what to change and propose a new search"
		if final == store.ApprovalExpired {
			msg = "no decision on the search within the waiting time; nothing executed"
		}
		return platform.Result{Status: "rejected", Message: msg}, nil
	}
	if err != nil {
		if errors.Is(err, store.ErrSearchRunning) {
			return searchErr("another training search is running in this chat")
		}
		return platform.Result{}, err
	}
	if !ok {
		return searchErr("search was decided elsewhere; nothing executed")
	}
	return platform.Result{Status: "ok", HTTPStatus: 200, Body: approvedSearchText(searchID, p, data)}, nil
}

// approvedSearchText is the answer to an approved proposal: the id and how to go on.
func approvedSearchText(id string, p search.Proposal, d SearchApprovalData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Search %s approved: %q, %s, objective %s (%s). ", id, d.Title, budgetText(d.Budget), p.Objective.Metric, p.Objective.Mode)
	b.WriteString("Declare the first trial with {{tool:search_next}} (search_id, params naming every parameter of the space, note: why this trial); ")
	b.WriteString("then make exactly the calls its answer names. They need no approval. After every trial the gateway tells you the result and you declare the next, ")
	b.WriteString("until the search ends. Never repeat a trial; read {{tool:search_status}} before you choose.")
	return b.String()
}

// templateBase is what the template way reads from the platform when it takes a proposal.
type templateBase struct {
	base  SearchBase
	fixed map[string]json.RawMessage
	raw   json.RawMessage // the stored form: {container_id, dataset_id, provider, architecture, config}
}

// storedBase is the JSON of searches.base on the template way.
type storedBase struct {
	ContainerID  int64                      `json:"container_id"`
	DatasetID    int64                      `json:"dataset_id"`
	Provider     string                     `json:"provider"`
	Architecture string                     `json:"architecture"`
	Config       map[string]json.RawMessage `json:"config"`
}

// readTemplateBase reads the base container: that it is this chat's, that it writes figures, its settings and
// schema. msg is a refusal for the agent; err a failure of the gateway.
func (m *Manager) readTemplateBase(ctx context.Context, chatID string, read platform.Doer, p search.Proposal) (templateBase, int64, string, error) {
	id := strconv.FormatInt(p.BaseContainerID, 10)
	own, err := m.st.IsDelegationObject(ctx, chatID, delegation.Training, id)
	if err != nil {
		return templateBase{}, 0, "", err
	}
	if !own {
		return templateBase{}, 0, fmt.Sprintf("training container %s is not one this chat made; build one with {{tool:create_training}} first", id), nil
	}
	if read == nil {
		return templateBase{}, 0, "the platform is not configured", nil
	}
	var mt struct {
		Supported bool `json:"supported"`
	}
	if msg := readJSONInto(ctx, read, "/train/containers/"+id+"/metrics", &mt); msg != "" {
		return templateBase{}, 0, "the training container could not be read: " + msg, nil
	}
	if !mt.Supported {
		return templateBase{}, 0, fmt.Sprintf("training container %s was built before the platform wrote figures; create a new one", id), nil
	}
	cfg, msg := readTrainConfig(ctx, read, id)
	if msg != "" {
		return templateBase{}, 0, "the configuration of the training container could not be read: " + msg, nil
	}
	if err := search.CheckSchema(p.Space, cfg.Config.Train.Schema); err != nil {
		return templateBase{}, 0, err.Error(), nil
	}
	b := templateBase{base: subjectOf(ctx, read, cfg, p.BaseContainerID), fixed: map[string]json.RawMessage{}}
	for k, v := range cfg.Config.Train.Values {
		if _, inSpace := p.Space[k]; !inSpace {
			b.fixed[k] = v
		}
	}
	b.raw, err = json.Marshal(storedBase{ContainerID: p.BaseContainerID, DatasetID: cfg.DatasetID, Provider: cfg.Provider,
		Architecture: cfg.Architecture.Name, Config: cfg.Config.Train.Values})
	return b, cfg.DatasetID, "", err
}

// readJSONInto reads a platform path and decodes the answer; a non-empty string is the reason it failed.
func readJSONInto(ctx context.Context, read platform.Doer, path string, out any) string {
	res, err := read(ctx, platform.Request{Method: "GET", Path: path})
	switch {
	case err != nil:
		return err.Error()
	case res.Status != "ok" || res.HTTPStatus >= 400:
		return fmt.Sprintf("%s %d", res.Status, res.HTTPStatus)
	}
	if err := json.Unmarshal([]byte(res.Body), out); err != nil {
		return "unreadable answer"
	}
	return ""
}

// readNotebookCode binds the files of the notebook way by SHA-256 (computed here from the bytes read), refuses a
// dataset that holds a test set, and builds the code part of the approval from the notebook service.
func (m *Manager) readNotebookCode(ctx context.Context, chatID string, read platform.Doer, p search.Proposal, req platform.Request) (store.SearchCode, json.RawMessage, int64, string) {
	if m.opt.Notebook == nil {
		return store.SearchCode{}, nil, 0, "the notebook service of this gateway is not available; use path template"
	}
	if len(req.Uploads) == 0 {
		return store.SearchCode{}, nil, 0, "files: at least one file is required for path notebook"
	}
	names := make([]string, 0, len(req.Uploads))
	sizes := make([]int64, 0, len(req.Uploads))
	code := store.SearchCode{Version: 1, Entry: p.Run.Entry, Args: p.Run.Args, DatasetIDs: p.Run.DatasetIDs, TimeoutMinutes: p.Run.TimeoutMinutes}
	for _, u := range req.Uploads {
		if u.Name == "params.json" {
			return store.SearchCode{}, nil, 0, "params.json is not one of the fixed files: you write it for every trial"
		}
		names = append(names, u.Name)
		sizes = append(sizes, int64(len(u.Data)))
		code.Files = append(code.Files, searchFileOf(u))
	}
	if err := platform.ValidateRunFiles(p.Run, names, sizes); err != nil {
		return store.SearchCode{}, nil, 0, err.Error()
	}
	if err := platform.CheckNotebookRun(p.Run, req.Uploads); err != nil {
		return store.SearchCode{}, nil, 0, err.Error()
	}
	var trainDS int64
	for _, id := range p.Run.DatasetIDs {
		if sp, err := m.st.SplitOfSourceDataset(ctx, chatID, id); err == nil {
			return store.SearchCode{}, nil, 0, fmt.Sprintf("dataset %d holds the held-back images of split %d; a search must not see them", id, sp.TrainDatasetID)
		}
		if _, err := m.st.SplitOfTrainDataset(ctx, chatID, id); err == nil && trainDS == 0 {
			trainDS = id
		}
	}
	runReq := platform.Request{Method: "POST", Path: platform.NotebookPrefix + "/runs", Form: platform.RunForm(p.Run), Uploads: req.Uploads}
	raw, err := m.opt.Notebook.ApprovalData(ctx, runReq, read)
	if err != nil {
		return store.SearchCode{}, nil, 0, err.Error()
	}
	return code, raw, trainDS, ""
}

// holdoutOf says whether the dataset the trials train on came from a split of this chat, so that its source holds
// the images the search never sees.
func (m *Manager) holdoutOf(ctx context.Context, chatID string, read platform.Doer, trainDataset int64) SearchHoldout {
	if trainDataset <= 0 {
		return SearchHoldout{}
	}
	sp, err := m.st.SplitOfTrainDataset(ctx, chatID, trainDataset)
	if err != nil || sp.SourceDatasetID <= 0 {
		return SearchHoldout{}
	}
	n := readNamed(ctx, read, sp.SourceDatasetID)
	return SearchHoldout{Available: true, EvalDataset: &n, Images: sp.KeptImages}
}

// searchRecheckState is what the recheck of a proposal keeps between clicks: the data and base the user saw last.
type searchRecheckState struct {
	mu   sync.Mutex
	data SearchApprovalData
	base json.RawMessage
}

func (c *searchRecheckState) baseNow() json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base
}

// searchRecheck reads the base container's settings again when the user clicks "Approve" (#154, DL2.1): a changed
// configuration gives a changed summary, which the user must see before the search starts; what is stored then is
// what the new summary says. The notebook way has nothing on the platform to re-read.
func (m *Manager) searchRecheck(chatID, apID string, p search.Proposal, cur *searchRecheckState) recheck {
	return func(ctx context.Context) ([]string, string, bool) {
		cur.mu.Lock()
		defer cur.mu.Unlock()
		if p.Path != search.PathTemplate {
			s := searchSummary(cur.data)
			return []string{s}, "", false
		}
		rl := &readLog{}
		read := m.loggedSummaryReader(chatID, rl)
		b, _, msg, err := m.readTemplateBase(ctx, chatID, read, p)
		m.logSummaryReads(ctx, chatID, apID, "approval", rl)
		if err != nil || msg != "" {
			// A platform that cannot be read right now must not block the user: approve what was stored.
			return []string{searchSummary(cur.data)}, "", true
		}
		fresh := cur.data
		fresh.Base, fresh.Fixed = &b.base, b.fixed
		s := searchSummary(fresh)
		if s != searchSummary(cur.data) {
			cur.data, cur.base = fresh, b.raw
			if raw, merr := json.Marshal(fresh); merr == nil {
				if _, rerr := m.st.ReplaceApprovalSearch(ctx, apID, raw); rerr != nil {
					slog.Error("search approval data not replaced", "approval", apID, "error", rerr)
				}
			}
		}
		return []string{s}, "", false
	}
}
