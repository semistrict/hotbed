package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// boardImportCmd imports channels, actors, schemas, messages, replies, tasks,
// dependencies, reactions, labels, and attachments from an existing @vercel/board
// HTTP server into Hotbed's native project-scoped BoardService.
type boardImportCmd struct {
	controllerClient
	URL             string   `env:"BOARD_URL" help:"Source @vercel/board base URL (default: $BOARD_URL, e.g. https://ubu1.tail2353d.ts.net/api/board)."`
	Token           string   `env:"BOARD_TOKEN" help:"Optional Bearer token for the source board."`
	Map             []string `name:"map" help:"Map a source channel to a Hotbed project: CHANNEL=PROJECT. Repeatable."`
	MapLabel        []string `name:"map-label" help:"Override target project for threads whose root has a matching label: KEY=VALUE:PROJECT (e.g. tag.hotbed=yes:hotbed). Replies inherit their root's routed project. Repeatable."`
	DefaultProject  string   `name:"default-project" help:"Fallback Hotbed project for channels not listed in --map and not matching an existing project name."`
	Channel         []string `name:"channel" help:"Only import these source channel(s). Repeatable (default: all channels)."`
	CreateProjects  bool     `name:"create-projects" help:"Automatically create missing Hotbed projects for unmapped channels."`
	SkipAttachments bool     `name:"skip-attachments" help:"Skip downloading and re-uploading attachment blobs."`
	BatchSize       int      `default:"100" help:"Number of messages per ImportBoard RPC batch."`
	DryRun          bool     `name:"dry-run" help:"Fetch and validate everything from the source board and print the import plan without writing to Hotbed."`
}

type boardImportSummary struct {
	SourceURL          string            `json:"sourceUrl"`
	DryRun             bool              `json:"dryRun"`
	ChannelMap         map[string]string `json:"channelMap"`
	ProjectCounts      map[string]int    `json:"projectCounts,omitempty"`
	Channels           int               `json:"channels"`
	ActorsFetched      int               `json:"actorsFetched"`
	SchemasFetched     int               `json:"schemasFetched"`
	MessagesFetched    int               `json:"messagesFetched"`
	TasksFetched       int               `json:"tasksFetched"`
	AttachmentsCopied  int               `json:"attachmentsCopied"`
	ActorsImported     int32             `json:"actorsImported"`
	SchemasImported    int32             `json:"schemasImported"`
	MessagesImported   int32             `json:"messagesImported"`
	MessagesSkipped    int32             `json:"messagesSkipped"`
	TasksImported      int32             `json:"tasksImported"`
	ProjectsCreated    int32             `json:"projectsCreated"`
	UnmappedChannels   []string          `json:"unmappedChannels,omitempty"`
}

// External @vercel/board JSON wire shapes.
type extBoardChannel struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Messages    int    `json:"messages"`
}

func (ch extBoardChannel) channelName() string {
	if ch.Path != "" {
		return ch.Path
	}
	return ch.Name
}

type extBoardActor struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Handle   string `json:"handle"`
	Name     string `json:"name"`
	Avatar   string `json:"avatar"`
	Verified bool   `json:"verified"`
	Via      *struct {
		Name string `json:"name"`
	} `json:"via"`
}

type extBoardSchema struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Version     int32           `json:"version"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	BuiltIn     bool            `json:"builtIn"`
	By          string          `json:"by"`
}

type extBoardAttachment struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

type extBoardTask struct {
	State   string          `json:"state"`
	Owner   string          `json:"owner"`
	By      string          `json:"by"`
	For     string          `json:"for"`
	Needs   []json.RawMessage `json:"needs"`
	Epoch   int32           `json:"epoch"`
	Result  string          `json:"result"`
	Reason  string          `json:"reason"`
	Claim   *struct {
		Actor string `json:"actor"`
		Until string `json:"until"`
	} `json:"claim"`
	Blocked *struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"blocked"`
}

type extBoardMessage struct {
	ID          json.RawMessage      `json:"id"`
	Channel     string               `json:"channel"`
	Text        string               `json:"text"`
	Re          json.RawMessage      `json:"re"`
	ReplyTo     json.RawMessage      `json:"replyTo"`
	Labels      map[string]string    `json:"labels"`
	Reactions   json.RawMessage      `json:"reactions"`
	By          json.RawMessage      `json:"by"`
	Author      *extBoardActor       `json:"author"`
	CreatedAt   string               `json:"createdAt"`
	EditedAt    string               `json:"editedAt"`
	Deleted     bool                 `json:"deleted"`
	Attachments []extBoardAttachment `json:"attachments"`
	Task        *extBoardTask        `json:"task"`
}

func extIDString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, "\"") {
		var out string
		if json.Unmarshal(raw, &out) == nil {
			return strings.TrimSpace(out)
		}
	}
	return s
}

func parseExtISO(raw string) int64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func extActorToPB(a *extBoardActor) *pb.BoardActor {
	if a == nil || a.ID == "" {
		return nil
	}
	kind := pb.BoardActorKind_BOARD_ACTOR_KIND_EXTERNAL
	switch strings.ToLower(a.Kind) {
	case "person", "user":
		kind = pb.BoardActorKind_BOARD_ACTOR_KIND_PERSON
	case "agent", "bot", "profile":
		kind = pb.BoardActorKind_BOARD_ACTOR_KIND_AGENT
	}
	viaName := ""
	if a.Via != nil {
		viaName = a.Via.Name
	}
	return &pb.BoardActor{
		Id:       a.ID,
		Kind:     kind,
		Handle:   a.Handle,
		Name:     a.Name,
		Avatar:   a.Avatar,
		ViaName:  viaName,
		Verified: a.Verified,
	}
}

func extAuthorToPB(rawBy json.RawMessage) *pb.BoardAuthor {
	s := strings.TrimSpace(string(rawBy))
	if s == "" || s == "null" {
		return nil
	}
	if strings.HasPrefix(s, "\"") {
		var actor string
		if json.Unmarshal(rawBy, &actor) == nil && actor != "" {
			principal, profile, _ := strings.Cut(actor, "+")
			return &pb.BoardAuthor{Actor: actor, Principal: principal, Profile: profile}
		}
		return nil
	}
	var obj struct {
		Actor     string `json:"actor"`
		Principal string `json:"principal"`
		Profile   string `json:"profile"`
	}
	if json.Unmarshal(rawBy, &obj) == nil && obj.Actor != "" {
		if obj.Principal == "" {
			obj.Principal, _, _ = strings.Cut(obj.Actor, "+")
		}
		return &pb.BoardAuthor{
			Actor:     obj.Actor,
			Principal: obj.Principal,
			Profile:   obj.Profile,
		}
	}
	return nil
}

func extReactionsToPB(raw json.RawMessage) []*pb.BoardReaction {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	// @vercel/board may represent reactions as map[emoji][]actor or []{emoji, actors/by}.
	var byMap map[string][]string
	if json.Unmarshal(raw, &byMap) == nil && len(byMap) > 0 {
		keys := make([]string, 0, len(byMap))
		for k := range byMap {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make([]*pb.BoardReaction, 0, len(keys))
		for _, k := range keys {
			out = append(out, &pb.BoardReaction{Emoji: k, Actors: byMap[k]})
		}
		return out
	}
	var arr []struct {
		Emoji  string   `json:"emoji"`
		Actors []string `json:"actors"`
		By     []string `json:"by"`
	}
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		out := make([]*pb.BoardReaction, 0, len(arr))
		for _, r := range arr {
			actors := r.Actors
			if len(actors) == 0 {
				actors = r.By
			}
			if r.Emoji != "" && len(actors) > 0 {
				out = append(out, &pb.BoardReaction{Emoji: r.Emoji, Actors: actors})
			}
		}
		return out
	}
	return nil
}

func (c *boardImportCmd) extGet(ctx context.Context, hc *http.Client, baseURL, path string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return nil, "", err
	}
	if tok := strings.TrimSpace(c.Token); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", api.Errorf(api.CodeInternal, "source board GET %s returned HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, resp.Header.Get("Content-Type"), nil
}

type labelProjectRule struct {
	key     string
	value   string
	project string
}

func parseMapLabels(raw []string) ([]labelProjectRule, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]labelProjectRule, 0, len(raw))
	for _, spec := range raw {
		lhs, proj, ok := strings.Cut(spec, ":")
		proj = strings.TrimSpace(proj)
		if !ok || proj == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "invalid --map-label %q: expected KEY=VALUE:PROJECT (e.g. tag.hotbed=yes:hotbed)", spec)
		}
		k, v, ok := strings.Cut(lhs, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "invalid --map-label %q: expected KEY=VALUE:PROJECT", spec)
		}
		out = append(out, labelProjectRule{key: k, value: v, project: proj})
	}
	return out, nil
}

func (c *boardImportCmd) Exec(ctx context.Context, d deps) (any, error) {
	sourceURL := strings.TrimSpace(c.URL)
	if sourceURL == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "source board URL is required (--url or BOARD_URL)")
	}
	sourceURL = strings.TrimRight(sourceURL, "/")
	explicitMap, err := parseBoardLabels(c.Map)
	if err != nil {
		return nil, err
	}
	if explicitMap == nil {
		explicitMap = map[string]string{}
	}
	labelRules, err := parseMapLabels(c.MapLabel)
	if err != nil {
		return nil, err
	}

	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}

	// Fetch existing Hotbed projects so same-name channels auto-map cleanly.
	projRes, err := cl.cluster.ListProjects(ctx, &pb.ListProjectsRequest{})
	if err != nil {
		return nil, err
	}
	existingProjects := make(map[string]bool, len(projRes.Projects))
	for _, p := range projRes.Projects {
		existingProjects[p.Name] = true
	}

	extClient := d.http
	if extClient == nil {
		extClient = http.DefaultClient
	}

	// 1. Fetch channels
	chRaw, _, err := c.extGet(ctx, extClient, sourceURL, "/channels")
	if err != nil {
		return nil, err
	}
	var chResp struct {
		Channels []extBoardChannel `json:"channels"`
	}
	if err := json.Unmarshal(chRaw, &chResp); err != nil {
		return nil, api.Errorf(api.CodeInternal, "decode /channels: %v", err)
	}

	channels := chResp.Channels
	if len(c.Channel) > 0 {
		filtered := channels[:0]
		for _, ch := range channels {
			if slices.Contains(c.Channel, ch.channelName()) {
				filtered = append(filtered, ch)
			}
		}
		channels = filtered
	}

	// Build channel -> project mapping
	channelMap := make(map[string]string, len(channels))
	var unmapped []string
	for _, ch := range channels {
		chName := ch.channelName()
		if chName == "" {
			continue
		}
		if target, ok := explicitMap[chName]; ok {
			channelMap[chName] = target
		} else if existingProjects[chName] {
			channelMap[chName] = chName
		} else if c.DefaultProject != "" {
			channelMap[chName] = c.DefaultProject
		} else if c.CreateProjects {
			channelMap[chName] = chName
		} else {
			unmapped = append(unmapped, chName)
		}
	}
	if len(unmapped) > 0 && !c.DryRun {
		return nil, api.Errorf(api.CodeInvalidArgs, "unmapped source channel(s): %s", strings.Join(unmapped, ", ")).
			WithSuggestion(fmt.Sprintf("pass --map %s=PROJECT, --default-project PROJECT, or --create-projects", unmapped[0]))
	}

	// 2. Fetch actors
	var actors []*pb.BoardActor
	if raw, _, aerr := c.extGet(ctx, extClient, sourceURL, "/actors"); aerr == nil {
		var aResp struct {
			Actors []extBoardActor `json:"actors"`
		}
		if json.Unmarshal(raw, &aResp) == nil {
			for i := range aResp.Actors {
				if pbA := extActorToPB(&aResp.Actors[i]); pbA != nil {
					actors = append(actors, pbA)
				}
			}
		}
	}

	// 3. Fetch custom schemas
	var schemas []*pb.CreateBoardSchemaRequest
	if raw, _, serr := c.extGet(ctx, extClient, sourceURL, "/schemas"); serr == nil {
		var sResp struct {
			Schemas []extBoardSchema `json:"schemas"`
		}
		if json.Unmarshal(raw, &sResp) == nil {
			for _, s := range sResp.Schemas {
				if s.BuiltIn || s.Name == "" || s.Version <= 0 {
					continue
				}
				schemas = append(schemas, &pb.CreateBoardSchemaRequest{
					Name:        s.Name,
					Version:     s.Version,
					Description: s.Description,
					SchemaJson:  string(s.Schema),
				})
			}
		}
	}

	// 4. Paginate all messages across selected channels from after=0 forward.
	// Note: in @vercel/board, omitting `after` returns only the newest page;
	// passing `after=0` walks chronologically from the very first message.
	var allMsgs []extBoardMessage
	seenMsgIDs := map[string]bool{}
	for _, ch := range channels {
		chName := ch.channelName()
		if chName == "" {
			continue
		}
		after := "0"
		for {
			q := url.Values{}
			q.Set("channel", chName)
			q.Set("limit", "200")
			q.Set("after", after)
			mRaw, _, err := c.extGet(ctx, extClient, sourceURL, "/messages?"+q.Encode())
			if err != nil {
				return nil, err
			}
			var mResp struct {
				Messages []extBoardMessage `json:"messages"`
				Cursor   json.RawMessage   `json:"cursor"`
			}
			if err := json.Unmarshal(mRaw, &mResp); err != nil {
				return nil, api.Errorf(api.CodeInternal, "decode /messages for %s: %v", chName, err)
			}
			if len(mResp.Messages) == 0 {
				break
			}
			added := 0
			lastID := ""
			for _, m := range mResp.Messages {
				if m.Channel == "" {
					m.Channel = chName
				}
				mid := extIDString(m.ID)
				if mid != "" {
					lastID = mid
					if seenMsgIDs[mid] {
						continue
					}
					seenMsgIDs[mid] = true
				}
				allMsgs = append(allMsgs, m)
				added++
			}
			nextAfter := lastID
			if cur := extIDString(mResp.Cursor); cur != "" {
				if curN, cerr := strconv.ParseInt(cur, 10, 64); cerr == nil {
					if lastN, lerr := strconv.ParseInt(lastID, 10, 64); lerr == nil && curN > lastN {
						nextAfter = cur
					}
				}
			}
			if nextAfter == "" || nextAfter == after || added == 0 {
				break
			}
			after = nextAfter
		}
	}

	// Sort chronologically by numeric ID (roots always before replies)
	slices.SortStableFunc(allMsgs, func(a, b extBoardMessage) int {
		ai, aerr := strconv.ParseInt(extIDString(a.ID), 10, 64)
		bi, berr := strconv.ParseInt(extIDString(b.ID), 10, 64)
		if aerr == nil && berr == nil {
			if ai < bi {
				return -1
			}
			if ai > bi {
				return 1
			}
			return 0
		}
		return strings.Compare(extIDString(a.ID), extIDString(b.ID))
	})

	// Resolve per-message target project: roots check --map-label overrides first,
	// else channelMap; replies inherit their root's target project.
	msgProject := make([]string, len(allMsgs))
	rootProject := make(map[string]string, len(allMsgs))
	projectCounts := map[string]int{}
	tasksCount := 0
	for i, m := range allMsgs {
		if m.Task != nil {
			tasksCount++
		}
		extID := extIDString(m.ID)
		reExt := extIDString(m.Re)
		proj := channelMap[m.Channel]
		if reExt != "" {
			if inherited, ok := rootProject[reExt]; ok && inherited != "" {
				proj = inherited
			}
		} else {
			for _, rule := range labelRules {
				if m.Labels[rule.key] == rule.value {
					proj = rule.project
					break
				}
			}
			if extID != "" && proj != "" {
				rootProject[extID] = proj
			}
		}
		msgProject[i] = proj
		if proj != "" {
			projectCounts[proj]++
		}
	}

	summary := boardImportSummary{
		SourceURL:        sourceURL,
		DryRun:           c.DryRun,
		ChannelMap:       channelMap,
		ProjectCounts:    projectCounts,
		Channels:         len(channels),
		ActorsFetched:    len(actors),
		SchemasFetched:   len(schemas),
		MessagesFetched:  len(allMsgs),
		TasksFetched:     tasksCount,
		UnmappedChannels: unmapped,
	}
	if c.DryRun {
		return summary, nil
	}

	// 5. Copy attachments (deduped by source attachment ID)
	attMap := map[string]*pb.BoardAttachment{}
	if !c.SkipAttachments {
		for _, m := range allMsgs {
			for _, att := range m.Attachments {
				if att.ID == "" {
					continue
				}
				if _, seen := attMap[att.ID]; seen {
					continue
				}
				data, ct, aerr := c.extGet(ctx, extClient, sourceURL, "/attachments/"+url.PathEscape(att.ID))
				if aerr != nil {
					continue
				}
				if att.ContentType != "" {
					ct = att.ContentType
				}
				uploaded, uerr := cl.board.UploadBoardAttachment(ctx, &pb.UploadBoardAttachmentRequest{
					Name:        att.Name,
					ContentType: ct,
					Data:        data,
				})
				if uerr != nil {
					return nil, uerr
				}
				attMap[att.ID] = uploaded
				summary.AttachmentsCopied++
			}
		}
	}

	// 6. Push in batches to ImportBoard
	batchSize := c.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}
	totalBatches := (len(allMsgs) + batchSize - 1) / batchSize
	if totalBatches == 0 {
		totalBatches = 1
	}

	for bIdx := range totalBatches {
		start := bIdx * batchSize
		end := min(start+batchSize, len(allMsgs))
		var pbMsgs []*pb.ImportedBoardMessage
		if start < len(allMsgs) {
			pbMsgs = make([]*pb.ImportedBoardMessage, 0, end-start)
			for j, m := range allMsgs[start:end] {
				extID := extIDString(m.ID)
				prefID, _ := strconv.ParseInt(extID, 10, 64)
				proj := msgProject[start+j]
				byPB := extAuthorToPB(m.By)
				authorCopy := m.Author
				if authorCopy != nil && authorCopy.ID == "" && byPB != nil {
					cp := *authorCopy
					cp.ID = byPB.Actor
					authorCopy = &cp
				}
				im := &pb.ImportedBoardMessage{
					ExternalId:        extID,
					PreferredId:       prefID,
					Project:           proj,
					Text:              m.Text,
					ReExternalId:      extIDString(m.Re),
					ReplyToExternalId: extIDString(m.ReplyTo),
					Labels:            m.Labels,
					Reactions:         extReactionsToPB(m.Reactions),
					By:                byPB,
					Author:            extActorToPB(authorCopy),
					CreatedAt:         parseExtISO(m.CreatedAt),
					Deleted:           m.Deleted,
				}
				if edited := parseExtISO(m.EditedAt); edited > 0 {
					im.EditedAt = &edited
				}
				for _, att := range m.Attachments {
					if mapped, ok := attMap[att.ID]; ok {
						im.Attachments = append(im.Attachments, mapped)
					}
				}
				if m.Task != nil {
					st, _ := parseBoardTaskState(m.Task.State)
					it := &pb.ImportedBoardTask{
						State:    st,
						Owner:    m.Task.Owner,
						By:       m.Task.By,
						ForActor: m.Task.For,
						Epoch:    m.Task.Epoch,
						Result:   m.Task.Result,
						Reason:   m.Task.Reason,
					}
					for _, rawNeed := range m.Task.Needs {
						if nid := extIDString(rawNeed); nid != "" {
							it.NeedsExternalIds = append(it.NeedsExternalIds, nid)
						}
					}
					if m.Task.Claim != nil {
						it.ClaimActor = m.Task.Claim.Actor
						it.ClaimUntil = parseExtISO(m.Task.Claim.Until)
					}
					if m.Task.Blocked != nil {
						it.BlockedKind = m.Task.Blocked.Kind
						it.BlockedReason = m.Task.Blocked.Reason
					}
					im.Task = it
				}
				pbMsgs = append(pbMsgs, im)
			}
		}

		req := &pb.ImportBoardRequest{
			Messages:       pbMsgs,
			CreateProjects: c.CreateProjects,
			FinalizeNeeds:  bIdx == totalBatches-1,
		}
		if bIdx == 0 {
			req.Actors = actors
			req.Schemas = schemas
		}
		res, err := cl.board.ImportBoard(ctx, req)
		if err != nil {
			return nil, err
		}
		summary.ActorsImported += res.ActorsImported
		summary.SchemasImported += res.SchemasImported
		summary.MessagesImported += res.MessagesImported
		summary.MessagesSkipped += res.MessagesSkipped
		summary.TasksImported += res.TasksImported
		summary.ProjectsCreated += res.ProjectsCreated
	}

	return summary, nil
}

func formatBoardImportSummary(s boardImportSummary) string {
	var b strings.Builder
	mode := "imported"
	if s.DryRun {
		mode = "dry-run"
	}
	fmt.Fprintf(&b, "board import (%s) from %s\n", mode, s.SourceURL)
	if len(s.ChannelMap) > 0 {
		keys := make([]string, 0, len(s.ChannelMap))
		for k := range s.ChannelMap {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s->%s", k, s.ChannelMap[k]))
		}
		fmt.Fprintf(&b, "  channels: %s\n", strings.Join(parts, ", "))
	}
	if len(s.ProjectCounts) > 0 {
		keys := make([]string, 0, len(s.ProjectCounts))
		for k := range s.ProjectCounts {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, s.ProjectCounts[k]))
		}
		fmt.Fprintf(&b, "  projects: %s\n", strings.Join(parts, ", "))
	}
	if len(s.UnmappedChannels) > 0 {
		fmt.Fprintf(&b, "  unmapped: %s (pass --map CH=PROJ or --create-projects)\n", strings.Join(s.UnmappedChannels, ", "))
	}
	if s.DryRun {
		fmt.Fprintf(&b, "  fetched:  %d messages (%d tasks), %d actors, %d custom schemas\n",
			s.MessagesFetched, s.TasksFetched, s.ActorsFetched, s.SchemasFetched)
	} else {
		fmt.Fprintf(&b, "  imported: %d messages (%d skipped), %d tasks, %d actors, %d schemas, %d attachments, %d projects created\n",
			s.MessagesImported, s.MessagesSkipped, s.TasksImported, s.ActorsImported, s.SchemasImported, s.AttachmentsCopied, s.ProjectsCreated)
	}
	return b.String()
}
