package twitter

import "time"

// TwitterUser represents a Twitter/X account profile.
type TwitterUser struct {
	ID          string
	Handle      string
	DisplayName string
	Bio         string
	Followers   int
	Following   int
	TweetCount  int
	ListedCount int
	CreatedAt   time.Time
	IsVerified  bool
	HasAvatar   bool
	HasBio      bool
}

// Tweet represents a single tweet.
type Tweet struct {
	ID            string
	AuthorID      string
	Text          string
	CreatedAt     time.Time
	Views         int
	Likes         int
	Retweets      int
	Quotes        int
	TokenMentions []string // extracted $TICKER patterns, e.g. ["BTC", "ETH"]
}

// Cursor is used for paginated GraphQL requests.
type Cursor struct {
	Value  string
	IsNext bool
}
