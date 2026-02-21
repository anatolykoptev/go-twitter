package twitter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var tokenMentionRe = regexp.MustCompile(`\$([A-Z]{2,10})`)

// parseUserByScreenName parses the UserByScreenName GraphQL response.
func parseUserByScreenName(body []byte) (*TwitterUser, error) {
	var raw struct {
		Data struct {
			User struct {
				Result userResult `json:"result"`
			} `json:"user"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal UserByScreenName: %w", err)
	}
	if len(raw.Errors) > 0 {
		return nil, fmt.Errorf("twitter API error: %s", raw.Errors[0].Message)
	}
	return parseUserResult(raw.Data.User.Result)
}

// parseUserList parses Followers/Following response.
func parseUserList(body []byte) ([]*TwitterUser, string, error) {
	var raw struct {
		Data struct {
			User struct {
				Result struct {
					Timeline struct {
						Timeline timelineObj `json:"timeline"`
					} `json:"timeline"`
				} `json:"result"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", fmt.Errorf("unmarshal user list: %w", err)
	}
	return extractUsersFromTimeline(raw.Data.User.Result.Timeline.Timeline)
}

// parseRetweeterList parses Retweeters response.
func parseRetweeterList(body []byte) ([]*TwitterUser, string, error) {
	var raw struct {
		Data struct {
			RetweetersTimeline struct {
				Timeline timelineObj `json:"timeline"`
			} `json:"retweeters_timeline"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", fmt.Errorf("unmarshal retweeter list: %w", err)
	}
	tl := raw.Data.RetweetersTimeline.Timeline
	if len(tl.Instructions) == 0 {
		return parseUserList(body)
	}
	return extractUsersFromTimeline(tl)
}

// parseTweetTimeline parses UserTweets timeline response.
func parseTweetTimeline(body []byte, authorID string) ([]*Tweet, error) {
	var raw struct {
		Data struct {
			User struct {
				Result struct {
					Timeline struct {
						Timeline timelineObj `json:"timeline"`
					} `json:"timeline"`
					TimelineV2 struct {
						Timeline timelineObj `json:"timeline"`
					} `json:"timeline_v2"`
				} `json:"result"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal tweet timeline: %w", err)
	}
	tl := raw.Data.User.Result.Timeline.Timeline
	if len(tl.Instructions) == 0 {
		tl = raw.Data.User.Result.TimelineV2.Timeline
	}
	return extractTweetsFromTimeline(tl, authorID)
}

// parseSearchTimeline parses SearchTimeline response.
func parseSearchTimeline(body []byte) ([]*Tweet, error) {
	var raw struct {
		Data struct {
			SearchByRawQuery struct {
				SearchTimeline struct {
					Timeline timelineObj `json:"timeline"`
				} `json:"search_timeline"`
			} `json:"search_by_raw_query"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal search timeline: %w", err)
	}
	return extractTweetsFromTimeline(raw.Data.SearchByRawQuery.SearchTimeline.Timeline, "")
}

// --- Timeline types ---

type timelineObj struct {
	Instructions []timelineInstruction `json:"instructions"`
}

type timelineInstruction struct {
	Type    string          `json:"type"`
	Entries []timelineEntry `json:"entries"`
	Entry   *timelineEntry  `json:"entry"`
}

type timelineEntry struct {
	EntryID   string          `json:"entryId"`
	SortIndex string          `json:"sortIndex"`
	Content   timelineContent `json:"content"`
}

type timelineContent struct {
	EntryType   string          `json:"entryType"`
	TypeName    string          `json:"__typename"`
	ItemContent json.RawMessage `json:"itemContent"`
	Value       string          `json:"value"`
	CursorType  string          `json:"cursorType"`
}

type userResult struct {
	TypeName string `json:"__typename"`
	ID       string `json:"id"`
	RestID   string `json:"rest_id"`
	Legacy   struct {
		Name            string `json:"name"`
		ScreenName      string `json:"screen_name"`
		FollowersCount  int    `json:"followers_count"`
		FriendsCount    int    `json:"friends_count"`
		StatusesCount   int    `json:"statuses_count"`
		ListedCount     int    `json:"listed_count"`
		CreatedAt       string `json:"created_at"`
		Verified        bool   `json:"verified"`
		Description     string `json:"description"`
		ProfileImageURL string `json:"profile_image_url_https"`
	} `json:"legacy"`
	IsBlueVerified bool `json:"is_blue_verified"`
}

type tweetResult struct {
	TypeName string `json:"__typename"`
	RestID   string `json:"rest_id"`
	Core     struct {
		UserResults struct {
			Result userResult `json:"result"`
		} `json:"user_results"`
	} `json:"core"`
	Legacy struct {
		FullText      string `json:"full_text"`
		CreatedAt     string `json:"created_at"`
		FavoriteCount int    `json:"favorite_count"`
		RetweetCount  int    `json:"retweet_count"`
		QuoteCount    int    `json:"quote_count"`
		UserIDStr     string `json:"user_id_str"`
	} `json:"legacy"`
	Views struct {
		Count string `json:"count"`
	} `json:"views"`
}

// --- Extraction helpers ---

func extractUsersFromTimeline(tl timelineObj) ([]*TwitterUser, string, error) {
	var users []*TwitterUser
	var nextCursor string

	for _, instruction := range tl.Instructions {
		entries := instruction.Entries
		if instruction.Entry != nil {
			entries = append(entries, *instruction.Entry)
		}
		for _, entry := range entries {
			if entry.Content.EntryType == "TimelineTimelineCursor" || entry.Content.TypeName == "TimelineTimelineCursor" {
				if entry.Content.CursorType == "Bottom" || strings.Contains(entry.EntryID, "cursor-bottom") {
					nextCursor = entry.Content.Value
				}
				continue
			}
			if entry.Content.ItemContent == nil {
				continue
			}
			var item struct {
				TypeName    string `json:"__typename"`
				UserResults struct {
					Result userResult `json:"result"`
				} `json:"user_results"`
			}
			if err := json.Unmarshal(entry.Content.ItemContent, &item); err != nil {
				continue
			}
			if item.TypeName != "TimelineUser" {
				continue
			}
			u, err := parseUserResult(item.UserResults.Result)
			if err != nil {
				slog.Debug("skip user parse error", slog.Any("error", err))
				continue
			}
			users = append(users, u)
		}
	}
	return users, nextCursor, nil
}

func extractTweetsFromTimeline(tl timelineObj, defaultAuthorID string) ([]*Tweet, error) {
	var tweets []*Tweet

	for _, instruction := range tl.Instructions {
		for _, entry := range instruction.Entries {
			if entry.Content.ItemContent == nil {
				continue
			}
			var item struct {
				TypeName     string `json:"__typename"`
				TweetResults struct {
					Result tweetResult `json:"result"`
				} `json:"tweet_results"`
			}
			if err := json.Unmarshal(entry.Content.ItemContent, &item); err != nil {
				continue
			}
			if item.TypeName != "TimelineTweet" {
				continue
			}
			t, err := parseTweetResult(item.TweetResults.Result, defaultAuthorID)
			if err != nil {
				slog.Debug("skip tweet parse error", slog.Any("error", err))
				continue
			}
			tweets = append(tweets, t)
		}
	}
	return tweets, nil
}

func parseUserResult(r userResult) (*TwitterUser, error) {
	if r.TypeName == "UserUnavailable" {
		return nil, fmt.Errorf("user unavailable (suspended or restricted)")
	}
	if r.RestID == "" {
		return nil, fmt.Errorf("empty user rest_id (typename=%s)", r.TypeName)
	}
	var createdAt time.Time
	if r.Legacy.CreatedAt != "" {
		t, err := time.Parse("Mon Jan 02 15:04:05 +0000 2006", r.Legacy.CreatedAt)
		if err == nil {
			createdAt = t
		}
	}
	bio := strings.TrimSpace(r.Legacy.Description)
	return &TwitterUser{
		ID:          r.RestID,
		Handle:      r.Legacy.ScreenName,
		DisplayName: r.Legacy.Name,
		Bio:         bio,
		Followers:   r.Legacy.FollowersCount,
		Following:   r.Legacy.FriendsCount,
		TweetCount:  r.Legacy.StatusesCount,
		ListedCount: r.Legacy.ListedCount,
		CreatedAt:   createdAt,
		IsVerified:  r.Legacy.Verified || r.IsBlueVerified,
		HasAvatar:   r.Legacy.ProfileImageURL != "" && !strings.Contains(r.Legacy.ProfileImageURL, "default_profile"),
		HasBio:      bio != "",
	}, nil
}

func parseTweetResult(r tweetResult, defaultAuthorID string) (*Tweet, error) {
	if r.RestID == "" {
		return nil, fmt.Errorf("empty tweet rest_id")
	}

	authorID := defaultAuthorID
	if r.Legacy.UserIDStr != "" {
		authorID = r.Legacy.UserIDStr
	}

	var createdAt time.Time
	if r.Legacy.CreatedAt != "" {
		t, err := time.Parse("Mon Jan 02 15:04:05 +0000 2006", r.Legacy.CreatedAt)
		if err == nil {
			createdAt = t
		}
	}

	views := 0
	if r.Views.Count != "" {
		views, _ = strconv.Atoi(r.Views.Count)
	}

	text := r.Legacy.FullText
	mentions := extractTokenMentions(text)

	return &Tweet{
		ID:            r.RestID,
		AuthorID:      authorID,
		Text:          text,
		CreatedAt:     createdAt,
		Views:         views,
		Likes:         r.Legacy.FavoriteCount,
		Retweets:      r.Legacy.RetweetCount,
		Quotes:        r.Legacy.QuoteCount,
		TokenMentions: mentions,
	}, nil
}

// parseCreateTweet extracts the tweet ID from a CreateTweet mutation response.
func parseCreateTweet(body []byte) (string, error) {
	var raw struct {
		Data struct {
			CreateTweet struct {
				TweetResults struct {
					Result struct {
						RestID string `json:"rest_id"`
					} `json:"result"`
				} `json:"tweet_results"`
			} `json:"create_tweet"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("unmarshal CreateTweet: %w", err)
	}
	if len(raw.Errors) > 0 {
		return "", fmt.Errorf("CreateTweet API error: %s", raw.Errors[0].Message)
	}
	tweetID := raw.Data.CreateTweet.TweetResults.Result.RestID
	if tweetID == "" {
		return "", fmt.Errorf("CreateTweet returned empty tweet ID: %s", truncateBytes(body, 300))
	}
	return tweetID, nil
}

func extractTokenMentions(text string) []string {
	matches := tokenMentionRe.FindAllStringSubmatch(strings.ToUpper(text), -1)
	seen := make(map[string]bool)
	var result []string
	for _, m := range matches {
		if len(m) >= 2 && !seen[m[1]] {
			seen[m[1]] = true
			result = append(result, m[1])
		}
	}
	return result
}
