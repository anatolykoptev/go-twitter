// Spike: which X source delivers new tweets fastest? (go-twitter #64 groundwork)
//
// Modes:
//
//	dryrun  — short check with 1 handle per arm (performs the setup writes
//	          for that handle; state keeps them for the full run).
//	run     — full measurement (default 125 min), all handles, cleanup at end.
//	cleanup — undo whatever state.json records (idempotent, safe to re-run).
//	report  — recompute stats from observations.jsonl.
//	resolve — resolve handle candidates to user IDs and print (no writes).
//
// Never-merged spike: lives under spike/freshness/, changes no library file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	twitter "github.com/anatolykoptev/go-twitter"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Account assignments (per spec; anatolykoptev/piteronlinetv/hullyday are
// brand accounts and never used).
const (
	armAAccount = "EvalikFulvik" // HomeLatestTimeline (+ device_follow probe)
	armBAccount = "AKoptev45254" // ListLatestTweetsTimeline + SearchTimeline
	armCAccount = "PiterPenhs"   // UserTweets round-robin
)

// targetCandidates: the 8 spec'd handles first, then substitutes for any that
// are protected/suspended/quiet (each substitution is logged).
var targetCandidates = []string{
	"FirstSquawk", "DeItaone", "Reuters", "business",
	"WSJ", "AP", "BBCBreaking", "CNBC",
	// substitutes
	"BreakingNews", "Bloomberg", "FT", "AJEnglish",
	"SkyNewsBreak", "ReutersWorld", "CNBCWorld", "WSJbreaking",
}

const (
	envPathDefault   = "/home/krolik/deploy/krolik-server/.env"
	writeBudget      = 20 // setup writes cap (matching undo excluded)
	followSpreadMin  = 10 * time.Minute
	listAddSpread    = 18 * time.Second
	unfollowSpread   = 45 * time.Second
	bellTargets      = 3 // bells enabled for at most this many handles
	listName         = "freshness-spike-srcs"
	defaultRunLength = 125 * time.Minute
	defaultDryLength = 5 * time.Minute
)

func main() {
	mode := flag.String("mode", "dryrun", "dryrun|run|cleanup|verify|report|resolve")
	out := flag.String("out", "/home/krolik/tmp/freshness-spike", "output dir")
	envPath := flag.String("env", envPathDefault, "krolik-server .env (encryption key source)")
	dur := flag.Duration("duration", 0, "measurement length (0 = mode default)")
	nHandles := flag.Int("handles", 0, "number of target handles (0 = mode default)")
	fracAB := flag.Float64("fraction-ab", 0.4, "fraction of observed rate-limit window used by arms A/B")
	fracC := flag.Float64("fraction-c", 0.8, "fraction of observed window used by arm C")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := *dur
	if d == 0 {
		switch *mode {
		case "run":
			d = defaultRunLength
		case "dryrun":
			d = defaultDryLength
		}
	}

	switch *mode {
	case "resolve":
		runResolve(ctx, *out, *envPath)
	case "dryrun", "run":
		n := *nHandles
		if n == 0 {
			if *mode == "dryrun" {
				n = 1
			} else {
				n = 8
			}
		}
		runMeasure(ctx, *out, *envPath, d, n, *fracAB, *fracC, *mode == "dryrun")
	case "cleanup":
		runCleanup(ctx, *out, *envPath)
	case "verify":
		runVerify(ctx, *out, *envPath)
	case "report":
		if err := runReport(*out); err != nil {
			slog.Error("report failed", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", *mode)
		os.Exit(2)
	}
}

// logTimesync captures the local clock state for the report.
func logTimesync(tag string) {
	out, err := exec.Command("timedatectl", "timesync-status").CombinedOutput()
	if err != nil {
		out, _ = exec.Command("timedatectl").CombinedOutput()
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		slog.Info("timesync "+tag, "line", l)
	}
}

// mkAccount builds the twitter.Account for one go-social account (browser
// profile assigned so UA + TLS fingerprint are consistent everywhere).
func mkAccount(acc *socialAccount, idx int) *twitter.Account {
	twAcc := &twitter.Account{
		Username:   acc.Handle,
		Password:   acc.Password,
		AuthToken:  acc.AuthToken,
		CT0:        acc.CT0,
		TOTPSecret: acc.TOTP,
		Proxy:      acc.Proxy,
	}
	twitter.AssignBrowserProfile(twAcc, idx)
	return twAcc
}

// buildTwitterClient builds a single-account client pinned to twAcc.
// The shared MeterProvider makes x-rate-limit-* headers observable via the
// client's ratelimit histograms (the public API otherwise discards headers).
func buildTwitterClient(twAcc *twitter.Account, mp *sdkmetric.MeterProvider) (*twitter.Client, error) {
	cli, err := twitter.NewClient(twitter.ClientConfig{
		Accounts:             []*twitter.Account{twAcc},
		DisableGuestFallback: true,
		SessionStore:         newMemSessionStore(),
		MeterProvider:        mp,
	})
	if err != nil {
		return nil, err
	}
	// Never relogin: a fresh login mints a new auth_token and can invalidate
	// the DB credentials production consumers (go-search via go-social) hold.
	cli.SetAutoReloginGate(noRelogin{})
	return cli, nil
}

// runResolve resolves candidates to user IDs via arm-B's account and prints
// them (user IDs are public data, not secrets).
func runResolve(ctx context.Context, out, envPath string) {
	_ = out
	key, err := loadEncryptionKey(envPath)
	if err != nil {
		fatal(err)
	}
	accs, err := loadAccounts(ctx, key, []string{armBAccount})
	if err != nil {
		fatal(err)
	}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	cli, err := buildTwitterClient(mkAccount(accs[armBAccount], 1), mp)
	if err != nil {
		fatal(err)
	}
	defer cli.Close()
	for _, h := range targetCandidates {
		u, err := cli.GetUserByScreenName(ctx, h)
		if err != nil {
			fmt.Printf("%-15s FAIL %v\n", h, err)
			continue
		}
		fmt.Printf("%-15s id=%s followers=%d tweets=%d\n", h, u.ID, u.Followers, u.TweetCount)
	}
}

// runMeasure is the dry-run and the full measurement.
func runMeasure(ctx context.Context, out, envPath string, dur time.Duration, nHandles int, fracAB, fracC float64, dry bool) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		fatal(err)
	}
	logTimesync("start")

	st, err := loadState(out)
	if err != nil {
		fatal(err)
	}
	if st.Phase == "cleanup_done" {
		st = &stateFile{Phase: "setup", Handles: map[string]string{}, Started: time.Now().UTC()}
	}

	key, err := loadEncryptionKey(envPath)
	if err != nil {
		fatal(err)
	}
	accs, err := loadAccounts(ctx, key, []string{armAAccount, armBAccount, armCAccount})
	if err != nil {
		fatal(err)
	}

	// Disable the three accounts for go-social leasing for the window —
	// records prior status so cleanup can restore it.
	disableAccounts(out, st, accs)

	w, err := newWriter(out)
	if err != nil {
		fatal(err)
	}
	defer w.close()

	// One MeterProvider for all three clients: the rlSampler sees every
	// response's x-rate-limit-* headers through their ratelimit histograms.
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	sampler := newRLSampler(reader, w)

	twA := mkAccount(accs[armAAccount], 0)
	twB := mkAccount(accs[armBAccount], 1)
	twC := mkAccount(accs[armCAccount], 2)
	cliA, err := buildTwitterClient(twA, mp)
	if err != nil {
		fatal(err)
	}
	defer cliA.Close()
	cliB, err := buildTwitterClient(twB, mp)
	if err != nil {
		fatal(err)
	}
	defer cliB.Close()
	cliC, err := buildTwitterClient(twC, mp)
	if err != nil {
		fatal(err)
	}
	defer cliC.Close()

	// Resolve handles (arm-B client), honoring any earlier resolved state.
	targets := resolveTargets(ctx, cliB, st, out, nHandles)
	if len(targets) == 0 {
		fatal(errors.New("no target handles resolved"))
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sessA, err := newXSession(accs[armAAccount], twA)
	if err != nil {
		fatal(err)
	}
	sessB, err := newXSession(accs[armBAccount], twB)
	if err != nil {
		fatal(err)
	}

	// Arm A: snapshot existing follows, then follow missing targets spread out.
	_, preExisting := snapshotFollowing(ctx, cliA, sessA, accs[armAAccount].Handle, targets, st, out)

	arms := map[string]*arm{
		"home":       newArm("home", armAAccount, "HomeLatestTimeline", targets),
		"list":       newArm("list", armBAccount, "ListLatestTweetsTimeline", targets),
		"search":     newArm("search", armBAccount, "SearchTimeline", targets),
		"usertweets": newArm("usertweets", armCAccount, "UserTweets", targets),
		"devfollow":  newArm("devfollow", armAAccount, "device_follow", targets),
	}

	// Coverage: search + usertweets cover all targets from t0; home covers
	// pre-existing follows now and new follows when they land; list covers
	// members as they are added; devfollow covers bell'd users.
	now := time.Now().UnixMilli()
	for h := range targets {
		arms["search"].setCoverSince(h, now)
		arms["usertweets"].setCoverSince(h, now)
	}
	belled, followed, members, _ := st.snapshot()
	for id := range preExisting {
		if h := handleForID(targets, id); h != id {
			arms["home"].setCoverSince(h, now)
		}
	}
	for _, uid := range followed {
		if h := handleForID(targets, uid); h != uid {
			arms["home"].setCoverSince(h, now)
		}
	}
	for _, uid := range members {
		if h := handleForID(targets, uid); h != uid {
			arms["list"].setCoverSince(h, now)
		}
	}
	for _, uid := range belled {
		if h := handleForID(targets, uid); h != uid {
			arms["devfollow"].setCoverSince(h, now)
		}
	}

	go sampler.run(probeCtx)

	// --- setup phase -------------------------------------------------------

	// Arm A follows: remaining targets spread over >= followSpreadMin for the
	// full run; the dry run has 1 handle and a short window, so it lands the
	// follow inside ~a minute (spread rule applies to the 8-handle run).
	followsPending := pendingTargets(targets, followed, preExisting)
	go func() {
		spread := followSpreadMin / time.Duration(max(1, len(followsPending)))
		if dry {
			spread = 60 * time.Second
		}
		for _, uid := range followsPending {
			if probeCtx.Err() != nil {
				return
			}
			// jittered wait BEFORE the write so follows land spread out
			jitterRange(probeCtx, spread/2, spread)
			if probeCtx.Err() != nil {
				return
			}
			if !writeAllowed(st) {
				slog.Warn("write budget exhausted; skipping follow", "uid", uid)
				return
			}
			res, err := sessA.follow(probeCtx, uid)
			w.recordWrite("home", "follow", uid, res, err)
			if err == nil && res.err() == nil {
				st.mutate(out, func(s *stateFile) {
					s.Followed = append(s.Followed, uid)
					s.Writes++
				})
				arms["home"].setCoverSince(handleForID(targets, uid), time.Now().UnixMilli())
				slog.Info("followed", "uid", uid)
			} else {
				slog.Warn("follow failed", "uid", uid, "err", fmt.Sprint(err, res.err()))
			}
		}
	}()

	// Arm B list: create (once) then add members ~listAddSpread apart.
	go func() {
		if st.getListID() == "" {
			if !jitterRange(probeCtx, 5*time.Second, 15*time.Second) {
				return
			}
			if !writeAllowed(st) {
				slog.Warn("write budget exhausted; list not created")
				return
			}
			id, err := sessB.listCreate(probeCtx, accs[armBAccount].Handle, listName)
			w.recordWrite("list", "list_create", id, xResult{}, err)
			if err != nil {
				slog.Error("list create failed", "err", err)
				return
			}
			st.mutate(out, func(s *stateFile) {
				s.ListID = id
				s.Writes++
			})
			slog.Info("list created", "id", id)
		}
		for _, uid := range pendingTargets(targets, members, nil) {
			if probeCtx.Err() != nil {
				return
			}
			jitterRange(probeCtx, listAddSpread/2, listAddSpread)
			if probeCtx.Err() != nil {
				return
			}
			if !writeAllowed(st) {
				slog.Warn("write budget exhausted; skipping list add", "uid", uid)
				return
			}
			listID := st.getListID()
			res, err := sessB.listAdd(probeCtx, listID, uid)
			w.recordWrite("list", "list_add", uid, res, err)
			if err == nil && res.err() == nil {
				st.mutate(out, func(s *stateFile) {
					s.ListMembers = append(s.ListMembers, uid)
					s.Writes++
				})
				arms["list"].setCoverSince(handleForID(targets, uid), time.Now().UnixMilli())
			} else {
				slog.Warn("list add failed", "uid", uid, "err", fmt.Sprint(err, res.err()))
			}
		}
	}()

	// Arm D probe: ONE read-only GET on device_follow.json per whole spike
	// (result cached in state — the full run does not re-probe after dry run).
	devCh := make(chan bool, 1)
	go func() {
		st.mu.Lock()
		probeDone := st.DevProbe != nil
		probeOK := probeDone && st.DevProbe.OK
		st.mu.Unlock()
		if probeDone {
			slog.Info("device_follow probe cached", "ok", probeOK)
			devCh <- probeOK
			return
		}
		if !jitterRange(probeCtx, 60*time.Second, 120*time.Second) {
			devCh <- false
			return
		}
		res, err := sessA.deviceFollowGet(probeCtx, 20)
		w.recordWrite("devfollow", "probe", deviceFollowURL, res, err)
		ok := err == nil && res.Status == 200
		st.mutate(out, func(s *stateFile) {
			s.DevProbe = &devProbe{Status: res.Status, OK: ok}
		})
		slog.Info("device_follow probe", "status", res.Status, "codes", res.Codes,
			"body", truncate(string(res.Body), 300))
		devCh <- ok
	}()

	// --- polling loops ------------------------------------------------------

	query := searchQuery(targets)
	go arms["search"].loop(probeCtx, w, &pacer{sampler, "SearchTimeline", fracAB, 5 * time.Second, 300 * time.Second, 30 * time.Second},
		func(ctx context.Context) ([]obsTweet, error) {
			ts, err := cliB.SearchTimeline(ctx, query, 50)
			if err != nil {
				return nil, err
			}
			return arms["search"].toObs(ts), nil
		})

	go arms["home"].loop(probeCtx, w, &pacer{sampler, "HomeLatestTimeline", fracAB, 5 * time.Second, 300 * time.Second, 30 * time.Second},
		func(ctx context.Context) ([]obsTweet, error) {
			ts, err := cliA.GetHomeLatestTimeline(ctx, 40)
			if err != nil {
				return nil, err
			}
			return arms["home"].toObs(ts), nil
		})

	// list loop starts once a list ID exists
	go func() {
		for probeCtx.Err() == nil && st.getListID() == "" {
			time.Sleep(2 * time.Second)
		}
		listID := st.getListID()
		if listID == "" {
			return
		}
		arms["list"].loop(probeCtx, w, &pacer{sampler, "ListLatestTweetsTimeline", fracAB, 5 * time.Second, 300 * time.Second, 30 * time.Second},
			func(ctx context.Context) ([]obsTweet, error) {
				ts, err := cliB.GetListTweets(ctx, listID, 20)
				if err != nil {
					return nil, err
				}
				return arms["list"].toObs(ts), nil
			})
	}()

	ids := make([]string, 0, len(targets))
	idToHandle := map[string]string{}
	for h, id := range targets {
		ids = append(ids, id)
		idToHandle[id] = h
	}
	go arms["usertweets"].userTweetsLoop(probeCtx, w,
		&pacer{sampler, "UserTweets", fracC, 15 * time.Second, 300 * time.Second, 60 * time.Second},
		cliC, ids, idToHandle)

	// Arm D: bells for up to bellTargets handles if the probe returned 200.
	// Full runs only — the dry run just records the probe result.
	go func() {
		probeOK := <-devCh
		if !probeOK || dry {
			return
		}
		enabled := 0
		for h, uid := range targets {
			if enabled >= bellTargets {
				break
			}
			belled, _, _, _ := st.snapshot()
			if contains(belled, uid) {
				continue
			}
			jitterRange(probeCtx, 20*time.Second, 60*time.Second)
			if probeCtx.Err() != nil {
				return
			}
			if !writeAllowed(st) {
				slog.Warn("write budget exhausted; skipping bell", "handle", h)
				return
			}
			res, err := sessA.bell(probeCtx, uid, true)
			w.recordWrite("devfollow", "bell_on", uid, res, err)
			if err == nil && res.err() == nil {
				st.mutate(out, func(s *stateFile) {
					s.Belled = append(s.Belled, uid)
					s.Writes++
				})
				arms["devfollow"].setCoverSince(h, time.Now().UnixMilli())
				enabled++
				slog.Info("bell on", "handle", h)
			} else {
				slog.Warn("bell on failed", "handle", h, "err", fmt.Sprint(err, res.err()))
			}
		}
		// start polling the endpoint once at least one bell is on
		for probeCtx.Err() == nil {
			b, _, _, _ := st.snapshot()
			if len(b) > 0 {
				break
			}
			time.Sleep(2 * time.Second)
		}
		b, _, _, _ := st.snapshot()
		if len(b) == 0 {
			return
		}
		arms["devfollow"].loop(probeCtx, w, &pacer{sampler, "device_follow", 0.4, 10 * time.Second, 300 * time.Second, 60 * time.Second},
			func(ctx context.Context) ([]obsTweet, error) {
				res, err := sessA.deviceFollowGet(ctx, 50)
				if err != nil {
					return nil, err
				}
				if res.Status != 200 {
					return nil, fmt.Errorf("device_follow status %d", res.Status)
				}
				return annotateTypes(deviceFollowTweets(res.Body), targets), nil
			})
	}()

	st.mutate(out, func(s *stateFile) { s.Phase = "measuring" })

	// Periodic materialization of the spec CSV.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-probeCtx.Done():
				return
			case <-t.C:
				dumpAllTweets(out, arms)
			}
		}
	}()

	slog.Info("measurement running", "duration", dur, "targets", len(targets), "dry", dry, "out", out)
	select {
	case <-time.After(dur):
	case <-ctx.Done():
	}
	// Stop all arm loops before touching X-side cleanup state.
	cancel()
	slog.Info("measurement window ended; cleaning up")

	// --- cleanup ------------------------------------------------------------
	// Dry runs keep their setup writes (the one follow + list + member carry
	// into the full run, saving write budget); they only restore leasing.
	if dry {
		reEnableAccounts(st)
	} else {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cleanupCancel()
		runCleanupOps(cleanupCtx, out, st, sessA, sessB, accs[armBAccount].Handle, w)
	}
	dumpAllTweets(out, arms)
	logTimesync("end")
	if dry {
		// "done_dry" keeps followed/list state on disk so the full run reuses
		// it — Phase "cleanup_done" would reset state and orphan the writes.
		st.mutate(out, func(s *stateFile) { s.Phase = "done_dry" })
	} else {
		st.mutate(out, func(s *stateFile) { s.Phase = "cleanup_done" })
	}
	slog.Info("done", "out", out, "dry", dry)
}

func fatal(err error) {
	slog.Error("fatal", "err", err)
	os.Exit(1)
}

// writeAllowed reports whether the setup-write budget still has room.
func writeAllowed(st *stateFile) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.Writes < writeBudget
}

// disableAccounts excludes the three accounts from go-social leasing for the
// window, recording prior status for restoration.
func disableAccounts(out string, st *stateFile, accs map[string]*socialAccount) {
	for _, name := range []string{armAAccount, armBAccount, armCAccount} {
		a := accs[name]
		aID, aStatus := a.ID, a.Status
		var already bool
		st.mutate(out, func(s *stateFile) {
			for _, as := range s.Accounts {
				if as.ID == aID {
					already = true
				}
			}
			if !already {
				s.Accounts = append(s.Accounts, accState{ID: aID, Username: name, PriorStatus: aStatus})
			}
		})
		if aStatus == "disabled" {
			continue // already excluded (e.g. re-run)
		}
		if err := setAccountStatus(context.Background(), aID, "disabled"); err != nil {
			slog.Error("disable account failed", "user", name, "err", err)
			continue
		}
		st.mutate(out, func(s *stateFile) {
			for i := range s.Accounts {
				if s.Accounts[i].ID == aID {
					s.Accounts[i].Disabled = true
				}
			}
		})
		slog.Info("account disabled for leasing", "user", name, "prior", aStatus)
	}
}

// reEnableAccounts restores prior statuses for accounts the probe disabled.
func reEnableAccounts(st *stateFile) {
	st.mu.Lock()
	acts := append([]accState(nil), st.Accounts...)
	st.mu.Unlock()
	for _, as := range acts {
		if !as.Disabled {
			continue
		}
		status := as.PriorStatus
		if status == "" || status == "disabled" {
			status = "healthy"
		}
		if err := setAccountStatus(context.Background(), as.ID, status); err != nil {
			slog.Error("re-enable account failed", "user", as.Username, "err", err)
			continue
		}
		st.mu.Lock()
		for i := range st.Accounts {
			if st.Accounts[i].ID == as.ID {
				st.Accounts[i].Disabled = false
			}
		}
		st.mu.Unlock()
		slog.Info("account re-enabled", "user", as.Username, "status", status)
	}
}

// resolveTargets resolves handle candidates to X user IDs; results are stored
// in state so a re-run doesn't re-resolve.
func resolveTargets(ctx context.Context, cli *twitter.Client, st *stateFile, out string, n int) map[string]string {
	targets := map[string]string{}
	st.mu.Lock()
	for h, id := range st.Handles {
		targets[h] = id
	}
	st.mu.Unlock()
	for _, h := range targetCandidates {
		if len(targets) >= n {
			break
		}
		hl := strings.ToLower(h)
		if _, ok := targets[hl]; ok {
			continue
		}
		u, err := cli.GetUserByScreenName(ctx, h)
		if err != nil {
			slog.Warn("handle resolution failed", "handle", h, "err", err)
			continue
		}
		if idTaken(targets, u.ID) {
			continue // already resolved under another spelling
		}
		targets[hl] = u.ID
		slog.Info("resolved handle", "handle", h, "id", u.ID, "followers", u.Followers, "primary", isPrimary(h, n))
	}
	st.mutate(out, func(s *stateFile) {
		for h, id := range targets {
			s.Handles[h] = id
		}
	})
	return targets
}

func idTaken(targets map[string]string, id string) bool {
	for _, u := range targets {
		if u == id {
			return true
		}
	}
	return false
}

func isPrimary(h string, n int) bool {
	for i := 0; i < n && i < 8; i++ {
		if strings.EqualFold(targetCandidates[i], h) {
			return true
		}
	}
	return false
}

// snapshotFollowing returns (selfUserID, set of already-followed target IDs).
// Primary path: v1.1 friends/ids.json via the account's own session (one
// request). Fallback: go-twitter's paginated GetFollowing, bounded to 90 s —
// in the dry run it retried internally for ~5 min then reported the
// Following window exhausted, which would have eaten the measurement window.
func snapshotFollowing(ctx context.Context, cli *twitter.Client, sess *xSession, selfHandle string, targets map[string]string, st *stateFile, out string) (string, map[string]bool) {
	pre := map[string]bool{}
	st.mu.Lock()
	selfID := st.SelfID
	st.mu.Unlock()
	if selfID == "" {
		u, err := cli.GetUserByScreenName(ctx, selfHandle)
		if err != nil {
			slog.Error("self resolution failed", "handle", selfHandle, "err", err)
			return "", pre
		}
		selfID = u.ID
		st.mutate(out, func(s *stateFile) { s.SelfID = selfID })
	}
	ids, err := sess.friendsIDs(ctx, selfID, 5000)
	if err != nil {
		slog.Warn("friends/ids failed; falling back to GetFollowing", "err", err)
		gctx, gcancel := context.WithTimeout(ctx, 90*time.Second)
		following, ferr := cli.GetFollowing(gctx, selfID, 500)
		gcancel()
		if ferr != nil {
			slog.Error("following snapshot failed (both paths)", "err", ferr)
			return selfID, pre
		}
		ids = make([]string, 0, len(following))
		for _, u := range following {
			ids = append(ids, u.ID)
		}
	}
	targetIDs := map[string]bool{}
	for _, id := range targets {
		targetIDs[id] = true
	}
	for _, id := range ids {
		if targetIDs[id] {
			pre[id] = true
		}
	}
	st.mutate(out, func(s *stateFile) { s.PreExisting = ids })
	slog.Info("following snapshot", "self", selfHandle, "total", len(ids), "targets_preexisting", len(pre))
	return selfID, pre
}

// pendingTargets lists target user IDs not in already and not in excluded.
func pendingTargets(targets map[string]string, already []string, excluded map[string]bool) []string {
	have := map[string]bool{}
	for _, id := range already {
		have[id] = true
	}
	var out []string
	for _, id := range targets {
		if have[id] || excluded[id] {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func handleForID(targets map[string]string, id string) string {
	for h, u := range targets {
		if u == id {
			return h
		}
	}
	return id
}

func searchQuery(targets map[string]string) string {
	names := make([]string, 0, len(targets))
	for h := range targets {
		names = append(names, h)
	}
	sort.Strings(names)
	for i, n := range names {
		names[i] = "from:" + n
	}
	return strings.Join(names, " OR ")
}

func annotateTypes(ts []obsTweet, targets map[string]string) []obsTweet {
	for i := range ts {
		if _, ok := targets[strings.ToLower(ts[i].Handle)]; ok {
			ts[i].Type = "original"
		} else {
			ts[i].Type = "foreign"
		}
	}
	return ts
}

func dumpAllTweets(out string, arms map[string]*arm) {
	var rows []*tweetRow
	for _, a := range arms {
		a.mu.Lock()
		for _, r := range a.seen {
			rows = append(rows, r)
		}
		a.mu.Unlock()
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Arm != rows[j].Arm {
			return rows[i].Arm < rows[j].Arm
		}
		return rows[i].CreatedMs < rows[j].CreatedMs
	})
	if err := dumpTweetsCSV(out, rows); err != nil {
		slog.Error("dump tweets.csv", "err", err)
	}
}

// runCleanupOps performs the undo work: bells off, unfollow added, delete
// list, re-enable accounts. Idempotent — each step only touches what state
// records. Shared between `run` completion and the `cleanup` mode.
// The list sweep also deletes any spike-named list state lost track of
// (a double-create orphaned one in the dry run).
func runCleanupOps(ctx context.Context, out string, st *stateFile, sessA, sessB *xSession, bScreen string, w *writer) {
	belled, followed, _, listID := st.snapshot()

	// Bells off first (undo of the last-added writes).
	for _, uid := range belled {
		if sessA == nil {
			break
		}
		res, err := sessA.bell(ctx, uid, false)
		w.recordWrite("devfollow", "bell_off", uid, res, err)
		jitterRange(ctx, 10*time.Second, 30*time.Second)
	}
	st.mutate(out, func(s *stateFile) { s.Belled = nil })

	// Unfollow only what we added — never pre-existing follows.
	for i, uid := range followed {
		if sessA == nil {
			break
		}
		res, err := sessA.unfollow(ctx, uid)
		w.recordWrite("home", "unfollow", uid, res, err)
		slog.Info("unfollow", "uid", uid, "n", i+1, "of", len(followed))
		jitterRange(ctx, unfollowSpread/2, unfollowSpread)
	}
	st.mutate(out, func(s *stateFile) { s.Followed = nil })

	if listID != "" && sessB != nil {
		if err := sessB.listDelete(ctx, listID); err != nil {
			slog.Error("list delete failed", "id", listID, "err", err)
		} else {
			slog.Info("list deleted", "id", listID)
			st.mutate(out, func(s *stateFile) { s.ListID = ""; s.ListMembers = nil })
		}
	}
	// Sweep any spike-named list the recorded id missed (e.g. a GraphQL
	// create that succeeded but returned an unparseable body).
	if sessB != nil {
		if owned, err := sessB.ownedLists(ctx, bScreen); err != nil {
			slog.Warn("list sweep: ownerships failed", "err", err)
		} else {
			for _, l := range owned {
				if l.Name != listName {
					continue
				}
				id := fmt.Sprintf("%d", l.ID)
				if err := sessB.listDelete(ctx, id); err != nil {
					slog.Error("list sweep delete failed", "id", id, "err", err)
				} else {
					slog.Info("list sweep deleted", "id", id)
					w.recordWrite("list", "list_delete", id, xResult{Status: 200}, nil)
				}
			}
		}
	}

	reEnableAccounts(st)
}

// runCleanup is the manual cleanup mode (safe to run repeatedly).
func runCleanup(ctx context.Context, out, envPath string) {
	st, err := loadState(out)
	if err != nil {
		fatal(err)
	}
	key, err := loadEncryptionKey(envPath)
	if err != nil {
		fatal(err)
	}
	accs, err := loadAccounts(ctx, key, []string{armAAccount, armBAccount, armCAccount})
	if err != nil {
		fatal(err)
	}
	sessA, err := newXSession(accs[armAAccount], mkAccount(accs[armAAccount], 0))
	if err != nil {
		fatal(err)
	}
	sessB, err := newXSession(accs[armBAccount], mkAccount(accs[armBAccount], 1))
	if err != nil {
		fatal(err)
	}
	w, err := newWriter(out)
	if err != nil {
		fatal(err)
	}
	defer w.close()
	runCleanupOps(ctx, out, st, sessA, sessB, accs[armBAccount].Handle, w)
	st.mutate(out, func(s *stateFile) { s.Phase = "cleanup_done" })
	slog.Info("cleanup done")
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
