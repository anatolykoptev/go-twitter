// Package model holds the response types returned by the go-twitter client.
// The root package re-exports each of them as a type alias, so importing this
// package directly is optional.
package model

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
	AuthorHandle  string // @screen_name (from core.user_results)
	AuthorName    string // display name (from core.user_results)
	Text          string
	CreatedAt     time.Time
	Views         int
	Likes         int
	Retweets      int
	Quotes        int
	ReplyCount    int
	TokenMentions []string // extracted $TICKER patterns, e.g. ["BTC", "ETH"]

	// Promoted is true for an entry X marks as promoted (a timeline ad). Ad IDs
	// are minted at serve time, so the freshness metric ignores them; consumers
	// filtering a timeline can drop them the same way.
	Promoted bool
}

// DMConversation is a single direct-message conversation from the 1.1 inbox.
// Participants holds the user IDs in the thread (the inbox shape carries
// participant IDs, not full user objects). Messages are the messages seen in the
// inbox_initial_state entries for this conversation.
type DMConversation struct {
	ConversationID string
	Participants   []string
	Messages       []DMMessage
}

// DMMessage is a single direct message. Fields mirror the 1.1 inbox
// message_data shape (id, sender_id, text, time as unix millis).
type DMMessage struct {
	ID             string
	ConversationID string
	SenderID       string
	Text           string
	CreatedAt      time.Time
}
