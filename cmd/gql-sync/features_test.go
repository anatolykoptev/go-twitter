package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

const (
	realChunk = "real/bundle.TweetEditHistory.86bb173a.js"
	realHome  = "real/home.html"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// realExtract runs the full feature pipeline (bundle names ∩ warm-page defaults)
// over the REAL captured x.com fixtures and returns the extracted {flag:bool}
// map plus the bundle names that had no warm-page default.
func realExtract(t *testing.T) (map[string]any, []string) {
	t.Helper()
	c := newFeatureCollector()
	c.consume(realChunk, readFixture(t, realChunk))
	defaults := parseFeatureDefaults(readFixture(t, realHome))
	return intersectFeatures(c.result(), defaults, quietLogger())
}

// committedBaseline is the 37-flag hand-maintained set shipped in features_gen.go,
// used to prove the diff/REMOVED behaviour over real data without coupling the
// test to the repo-root file. Values are the committedFeatures() defaults.
var committedBaseline = map[string]bool{
	"articles_preview_enabled":                                                true,
	"c9s_tweet_anatomy_moderator_badge_enabled":                               true,
	"communities_web_enable_tweet_community_results_fetch":                    true,
	"content_disclosure_ai_generated_indicator_enabled":                       true,
	"content_disclosure_indicator_enabled":                                    true,
	"creator_subscriptions_tweet_preview_api_enabled":                         true,
	"freedom_of_speech_not_reach_fetch_enabled":                               true,
	"graphql_is_translatable_rweb_tweet_is_translatable_enabled":              true,
	"longform_notetweets_consumption_enabled":                                 true,
	"longform_notetweets_inline_media_enabled":                                true,
	"longform_notetweets_rich_text_read_enabled":                              true,
	"post_ctas_fetch_enabled":                                                 true,
	"premium_content_api_read_enabled":                                        false,
	"profile_label_improvements_pcf_label_in_post_enabled":                    true,
	"responsive_web_edit_tweet_api_enabled":                                   true,
	"responsive_web_enhance_cards_enabled":                                    false,
	"responsive_web_graphql_exclude_directive_enabled":                        true,
	"responsive_web_graphql_skip_user_profile_image_extensions_enabled":       false,
	"responsive_web_graphql_timeline_navigation_enabled":                      true,
	"responsive_web_grok_analyze_button_fetch_trends_enabled":                 true,
	"responsive_web_grok_analyze_post_followups_enabled":                      true,
	"responsive_web_grok_analysis_button_from_backend":                        true,
	"responsive_web_grok_annotations_enabled":                                 true,
	"responsive_web_grok_community_note_auto_translation_is_enabled":          true,
	"responsive_web_grok_image_annotation_enabled":                            true,
	"responsive_web_grok_imagine_annotation_enabled":                          true,
	"responsive_web_grok_share_attachment_enabled":                            true,
	"responsive_web_grok_show_grok_translated_post":                           true,
	"responsive_web_jetfuel_frame":                                            true,
	"responsive_web_profile_redirect_enabled":                                 true,
	"responsive_web_twitter_article_tweet_consumption_enabled":                true,
	"rweb_tipjar_consumption_enabled":                                         true,
	"rweb_video_screen_enabled":                                               true,
	"standardized_nudges_misinfo":                                             true,
	"tweet_with_visibility_results_prefer_gql_limited_actions_policy_enabled": true,
	"verified_phone_label_enabled":                                            false,
	"view_counts_everywhere_api_enabled":                                      true,
}

// TestExtractFeatures_RealFixture proves the extractor recovers the {flag:bool}
// map from the REAL captured bundle + warm page — NOT a synthetic fixture. It
// asserts the exact bool default for flags whose live default DIFFERS from the
// committed baseline, the precise surprise a synthetic fixture would have hidden.
func TestExtractFeatures_RealFixture(t *testing.T) {
	got, noDefault := realExtract(t)

	// Every committed flag the bundle still declares must be recovered.
	for name, want := range committedBaseline {
		if name == "responsive_web_graphql_exclude_directive_enabled" {
			continue // genuinely absent from the bundle — see TestFeatures_RemovedSurfaced
		}
		v, ok := got[name]
		if !ok {
			t.Errorf("committed flag %q not recovered from real fixture", name)
			continue
		}
		_ = want // committed value is NOT asserted equal — live defaults legitimately differ
		if _, isBool := v.(bool); !isBool {
			t.Errorf("flag %q: non-bool value %v", name, v)
		}
	}

	// Live defaults that DIFFER from the committed baseline (the human-eyeball set).
	liveFalse := []string{
		"longform_notetweets_inline_media_enabled",
		"post_ctas_fetch_enabled",
		"responsive_web_grok_analyze_button_fetch_trends_enabled",
		"responsive_web_grok_analyze_post_followups_enabled",
		"responsive_web_profile_redirect_enabled",
		"rweb_tipjar_consumption_enabled",
		"rweb_video_screen_enabled",
	}
	for _, name := range liveFalse {
		if got[name] != false {
			t.Errorf("flag %q: live default = %v, want false (committed had true)", name, got[name])
		}
	}

	// Newly added flags the bundle declares that the committed baseline lacks.
	for _, name := range []string{"rweb_cashtags_enabled", "rweb_cashtags_composer_attachment_enabled"} {
		if _, ok := got[name]; !ok {
			t.Errorf("new bundle flag %q not extracted", name)
		}
	}

	// A bundle name with no warm-page default is skipped (not invented), surfaced.
	if len(noDefault) != 1 || noDefault[0] != "rweb_conversational_replies_downvote_enabled" {
		t.Errorf("noDefault = %v, want [rweb_conversational_replies_downvote_enabled]", noDefault)
	}
	if _, leaked := got["rweb_conversational_replies_downvote_enabled"]; leaked {
		t.Error("undefaulted flag leaked into the extracted map with an invented value")
	}
}

// TestParseFeatureDefaults_IgnoresNonBool proves the default parser only picks up
// the "flag":{"value":bool} shape and never the array-valued switches (e.g. the
// country whitelist) that share the defaultConfig object.
func TestParseFeatureDefaults_IgnoresNonBool(t *testing.T) {
	defaults := parseFeatureDefaults(readFixture(t, realHome))
	if _, ok := defaults["account_country_setting_countries_whitelist"]; ok {
		t.Error("array-valued switch wrongly parsed as a boolean default")
	}
	if v, ok := defaults["responsive_web_graphql_timeline_navigation_enabled"]; !ok || v != true {
		t.Errorf("timeline_navigation default = %v,%v want true,true", v, ok)
	}
}

// TestFeatures_DiffMerge proves the union semantics: extracted defaults win on
// overlap, a new extracted flag is added, and a baseline flag absent from the
// extraction is surfaced as REMOVED (never silently dropped).
func TestFeatures_DiffMerge(t *testing.T) {
	baseline := map[string]bool{
		"shared_flag":   true,  // present in both — extracted value wins
		"removed_flag":  false, // baseline only — must surface as REMOVED
		"another_kept":  true,
		"another_kept2": false,
	}
	extracted := map[string]any{
		"shared_flag":   false, // flipped vs baseline
		"another_kept":  true,
		"another_kept2": false,
		"new_flag":      true, // extraction only — added
	}

	removed := removedFeatures(extracted, baseline)
	if len(removed) != 1 || !mapHasKey(removed, "removed_flag") {
		t.Fatalf("removed = %v, want only removed_flag", removed)
	}
	// removed_flag must NOT be in the live map.
	if _, ok := extracted["removed_flag"]; ok {
		t.Fatal("removed flag must not be a live entry")
	}

	out, err := renderFeatures(extracted, removed, "")
	if err != nil {
		t.Fatalf("renderFeatures: %v", err)
	}
	src := string(out)

	// Parse the live map back: extracted defaults win, new flag added, removed
	// flag is NOT a live entry. (gofmt column-aligns, so verify via the parser
	// rather than brittle single-space substring matches.)
	live := parseFeatureBaseline(out)
	if live["shared_flag"] != false {
		t.Errorf("shared_flag live = %v, want false (extracted wins)", live["shared_flag"])
	}
	if live["new_flag"] != true {
		t.Errorf("new_flag missing/false in live map: %v", live)
	}
	if _, ok := live["removed_flag"]; ok {
		t.Errorf("removed flag leaked as a live map entry\n%s", src)
	}

	// REMOVED flag surfaces only as a comment line.
	for _, want := range []string{"// REMOVED", "//\tremoved_flag (was false)"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("rendered output missing %q\n%s", want, src)
		}
	}
}

// TestFeatures_RemovedSurfaced proves the real-data REMOVED case end-to-end: the
// committed baseline carries responsive_web_graphql_exclude_directive_enabled,
// which the live bundle no longer declares; it must surface as a REMOVED comment.
func TestFeatures_RemovedSurfaced(t *testing.T) {
	extracted, _ := realExtract(t)
	removed := removedFeatures(extracted, committedBaseline)
	if !mapHasKey(removed, "responsive_web_graphql_exclude_directive_enabled") {
		t.Fatalf("exclude_directive (absent from real bundle) not surfaced as REMOVED; removed=%v", removed)
	}
}

// TestParseFeatureBaseline_RoundTrip proves the baseline parser reads back the
// emitter's own output and ignores the REMOVED comment lines.
func TestParseFeatureBaseline_RoundTrip(t *testing.T) {
	extracted := map[string]any{"a_enabled": true, "b_enabled": false}
	removed := map[string]bool{"gone_enabled": true}
	out, err := renderFeatures(extracted, removed, "")
	if err != nil {
		t.Fatalf("renderFeatures: %v", err)
	}
	baseline := parseFeatureBaseline(out)
	if len(baseline) != 2 || baseline["a_enabled"] != true || baseline["b_enabled"] != false {
		t.Fatalf("baseline = %v, want {a_enabled:true,b_enabled:false}", baseline)
	}
	if _, ok := baseline["gone_enabled"]; ok {
		t.Error("REMOVED comment line wrongly parsed as a live baseline entry")
	}
}

// TestRenderFeatures_Golden proves the emitter output over the REAL extracted set
// (plus the real REMOVED flag) is byte-identical to the committed golden.
func TestRenderFeatures_Golden(t *testing.T) {
	extracted, _ := realExtract(t)
	removed := removedFeatures(extracted, committedBaseline)
	got, err := renderFeatures(extracted, removed, "")
	if err != nil {
		t.Fatalf("renderFeatures: %v", err)
	}

	goldenPath := filepath.Join("testdata", "features_gen.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("emitter output != golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRun_RealRefreshesBoth proves acceptance #1: a single gql-sync invocation
// over the real fixtures refreshes BOTH queryids_gen.go AND features_gen.go.
func TestRun_RealRefreshesBoth(t *testing.T) {
	out := t.TempDir()
	if err := run([]string{"-fixtures", filepath.Join("testdata", "real"), "-out", out}); err != nil {
		t.Fatalf("run: %v", err)
	}

	qids, err := os.ReadFile(filepath.Join(out, generatedFileName))
	if err != nil {
		t.Fatalf("queryids_gen.go not written: %v", err)
	}
	if !bytes.Contains(qids, []byte("TweetEditHistory")) {
		t.Errorf("queryids_gen.go missing an extracted op:\n%s", qids)
	}

	feats, err := os.ReadFile(filepath.Join(out, featuresFileName))
	if err != nil {
		t.Fatalf("features_gen.go not written: %v", err)
	}
	live := parseFeatureBaseline(feats)
	if len(live) != 38 {
		t.Errorf("features_gen.go has %d flags, want 38", len(live))
	}
	if live["view_counts_everywhere_api_enabled"] != true ||
		live["verified_phone_label_enabled"] != false {
		t.Errorf("unexpected live defaults: %v", live)
	}
}

func mapHasKey(m map[string]bool, k string) bool {
	_, ok := m[k]
	return ok
}
