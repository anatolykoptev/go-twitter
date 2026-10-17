package main

// `verify` mode: post-cleanup evidence probes. READ-ONLY — it performs no
// writes on X and no status changes in go-social. Prints:
//   - EvalikFulvik's live follow set vs the expected pre-existing set
//   - bell (notifications) state for each uid the probe ever bell'd
//   - the deleted list's lookup result + AKoptev45254's owned lists
//   - the three accounts' current go-social status vs recorded prior status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

func runVerify(ctx context.Context, out, envPath string) {
	st, err := loadState(out)
	if err != nil {
		fatal(err)
	}
	key, err := loadEncryptionKey(envPath)
	if err != nil {
		fatal(err)
	}
	accs, err := loadAccounts(ctx, key, []string{armAAccount, armBAccount})
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
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	cliA, err := buildTwitterClient(mkAccount(accs[armAAccount], 0), mp)
	if err != nil {
		fatal(err)
	}
	defer cliA.Close()

	// --- 1. follow set ------------------------------------------------------
	fmt.Println("== EvalikFulvik follow set ==")
	selfID := st.SelfID
	if selfID == "" {
		u, err := cliA.GetUserByScreenName(ctx, armAAccount)
		if err != nil {
			fatal(fmt.Errorf("self resolve: %w", err))
		}
		selfID = u.ID
	}
	live, err := sessA.friendsIDs(ctx, selfID, 5000)
	if err != nil {
		fmt.Println("friends/ids FAILED:", err)
	} else {
		targetIDs := map[string]bool{}
		for _, id := range st.Handles {
			targetIDs[id] = true
		}
		// Expected = recorded pre-existing minus any target ids. (FirstSquawk
		// appears in PreExisting only because the dry run's follow persisted;
		// it is still a target and must be gone.)
		var expected []string
		for _, id := range st.PreExisting {
			if !targetIDs[id] {
				expected = append(expected, id)
			}
		}
		sort.Strings(expected)
		sort.Strings(live)
		fmt.Printf("expected (%d): %s\n", len(expected), strings.Join(expected, " "))
		fmt.Printf("live     (%d): %s\n", len(live), strings.Join(live, " "))
		liveSet := map[string]bool{}
		for _, id := range live {
			liveSet[id] = true
		}
		var extra []string
		for _, id := range live {
			if targetIDs[id] {
				extra = append(extra, id+"(target!)")
			}
		}
		expSet := map[string]bool{}
		for _, id := range expected {
			expSet[id] = true
			if !liveSet[id] {
				extra = append(extra, id+"(missing-preexisting!)")
			}
		}
		for _, id := range live {
			if !expSet[id] && !targetIDs[id] {
				extra = append(extra, id+"(unexpected)")
			}
		}
		if len(extra) == 0 {
			fmt.Println("RESULT: OK — follow set matches pre-existing, no targets followed")
		} else {
			fmt.Println("RESULT: MISMATCH —", strings.Join(extra, " "))
		}
	}

	// --- 2. bell state ------------------------------------------------------
	fmt.Println("\n== bell state (friendships/show on bell'd uids) ==")
	belled, _, _, _ := st.snapshot()
	// state.Belled is cleared after cleanup; check every uid we ever bell'd
	// (recorded in writes.csv) plus, defensively, all targets.
	check := map[string]bool{}
	for _, id := range belled {
		check[id] = true
	}
	for _, id := range st.Handles { // defensive: bell state for all targets
		check[id] = true
	}
	var notifOn []string
	for id := range check {
		fields, err := sessA.friendshipShow(ctx, selfID, id)
		if err != nil {
			fmt.Printf("  uid=%s show FAILED: %v\n", id, err)
			continue
		}
		fmt.Printf("  uid=%-12s %s\n", id, fields)
		if strings.Contains(fields, "notifications_enabled=true") ||
			strings.Contains(fields, "device_following=true") {
			notifOn = append(notifOn, id)
		}
	}
	if len(notifOn) == 0 {
		fmt.Println("RESULT: OK — no target has notifications on")
	} else {
		fmt.Println("RESULT: BELLS STILL ON —", strings.Join(notifOn, " "))
	}

	// --- 3. list state ------------------------------------------------------
	fmt.Println("\n== list state ==")
	_, _, _, listID := st.snapshot()
	fmt.Printf("state.json list_id: %q (empty = probe deleted it)\n", listID)
	if listID == "" {
		listID = deletedListHint
	}
	res, err := sessB.listShow(ctx, listID)
	if err != nil {
		fmt.Printf("  lists/show id=%s transport err: %v\n", listID, err)
	} else {
		fmt.Printf("  lists/show id=%s → status %d codes %v body %.160s\n",
			listID, res.Status, res.Codes, string(res.Body))
	}
	owned, err := sessB.ownedLists(ctx, armBAccount)
	if err != nil {
		fmt.Println("  lists/ownerships FAILED:", err)
	} else {
		desc := make([]string, 0, len(owned))
		for _, l := range owned {
			desc = append(desc, fmt.Sprintf("%s(id=%d,%s,members=%d)", l.Name, l.ID, l.Mode, l.Members))
		}
		fmt.Printf("  AKoptev45254 owns %d lists: %s\n", len(owned), strings.Join(desc, " "))
		if len(owned) == 0 {
			fmt.Println("RESULT: OK — no lists remain")
		}
	}

	// --- 4. go-social status ------------------------------------------------
	fmt.Println("\n== go-social account status ==")
	for _, as := range st.Accounts {
		cur, err := getAccountStatus(ctx, as.ID)
		if err != nil {
			fmt.Printf("  %-15s query FAILED: %v\n", as.Username, err)
			continue
		}
		mark := "OK"
		if cur != as.PriorStatus {
			mark = "MISMATCH"
		}
		fmt.Printf("  %-15s prior=%-9s now=%-9s %s\n", as.Username, as.PriorStatus, cur, mark)
	}
}

// deletedListHint is the list ID from the run's writes (state clears it on
// successful delete); kept for the verify lookup only.
const deletedListHint = "2111299249170461127"

// friendshipShow GETs friendships/show.json and returns the source
// relationship's bell-relevant fields as "k=v k=v".
func (x *xSession) friendshipShow(ctx context.Context, sourceID, targetID string) (string, error) {
	q := url.Values{"source_id": {sourceID}, "target_id": {targetID}}
	res, err := x.do(ctx, "GET", "https://x.com/i/api/1.1/friendships/show.json?"+q.Encode(),
		"application/x-www-form-urlencoded", nil)
	if err != nil {
		return "", err
	}
	if res.Status != 200 {
		return "", res.err()
	}
	var parsed struct {
		Relationship struct {
			Source map[string]any `json:"source"`
		} `json:"relationship"`
	}
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return "", err
	}
	var parts []string
	for k, v := range parsed.Relationship.Source {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "notif") || strings.Contains(lk, "device") ||
			strings.Contains(lk, "follow") || strings.Contains(lk, "bell") {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "(no notification/following fields in response)", nil
	}
	return strings.Join(parts, " "), nil
}

// listShow GETs lists/show.json for one list ID.
func (x *xSession) listShow(ctx context.Context, listID string) (xResult, error) {
	q := url.Values{"list_id": {listID}}
	return x.do(ctx, "GET", "https://x.com/i/api/1.1/lists/show.json?"+q.Encode(),
		"application/x-www-form-urlencoded", nil)
}

// getAccountStatus reads one go-social account's current status.
func getAccountStatus(ctx context.Context, id string) (string, error) {
	out, err := psql(ctx, `SELECT status FROM accounts WHERE id=`+quoteSQL(id))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
