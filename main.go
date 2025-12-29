package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB
var gymChannelID string

func main() {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("Error: DISCORD_BOT_TOKEN environment variable is not set")
	}

	gymChannelID = os.Getenv("GYM_CHANNEL_ID")
	if gymChannelID == "" {
		log.Fatal("Error: GYM_CHANNEL_ID environment variable is not set")
	}

	var err error
	// Add WAL mode and other optimizations directly in connection string
	db, err = sql.Open("sqlite3", "file:./gym.db?cache=shared&_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	err = initializeDb(db)
	if err != nil {
		log.Fatal(err)
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal(err)
	}

	// Register Handlers
	dg.AddHandler(onInteraction)

	err = dg.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer dg.Close()

	// Register commands
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "gym",
			Description: "Log a gym session",
		},
		{
			Name:        "leaderboard",
			Description: "Show the top gym-goers",
		},
		{
			Name:        "insult",
			Description: "Get a gym-related insult",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "user",
					Description: "The user you want to roast",
					Required:    true,
				},
			},
		},
	}

	for _, v := range commands {
		_, err := dg.ApplicationCommandCreate(dg.State.User.ID, "", v)
		if err != nil {
			log.Panicf("Cannot create '%v' command: %v", v.Name, err)
		}
	}

	fmt.Println("Gym Bot is active. Press CTRL+C to stop.")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}

func onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.ChannelID != gymChannelID {
		sendResponse(s, i, "❌ Commands can only be used in the designated gym channel.")
		return
	}

	switch i.ApplicationCommandData().Name {
	case "gym":
		handleGymLog(s, i)
	case "leaderboard":
		handleLeaderboard(s, i)
	case "insult":
		handleInsult(s, i)
	}
}

func handleInsult(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// 1. Tell Discord to wait (The "Bot is thinking..." state)
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("Error deferring: %v", err)
		return
	}

	// Check if a username option is provided
	options := i.ApplicationCommandData().Options
	var targetUser *discordgo.User

	if len(options) > 0 && options[0].Name == "user" {
		// If a user is specified, use that user
		targetUser = options[0].UserValue(s)
	} else {
		// Default to the user who invoked the command
		targetUser = i.Member.User
	}

	// Use context with timeout for better control
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Query to calculate days since last workout and workouts this week
	var daysSinceLastWorkout int
	var workoutsThisWeek int
	err = db.QueryRowContext(ctx, `
		SELECT 
			COALESCE(CAST(julianday('now') - julianday(MAX(timestamp)) AS INT), -1) AS days_since_last,
			COALESCE(SUM(CASE WHEN strftime('%W', timestamp) = strftime('%W', 'now') THEN 1 ELSE 0 END), 0) AS workouts_this_week
		FROM workouts
		WHERE user_id = ?
	`, targetUser.ID).Scan(&daysSinceLastWorkout, &workoutsThisWeek)

	if err != nil {
		sendResponse(s, i, "❌ Could not retrieve workout data.")
		log.Printf("Error querying workout data for user %s: %v", targetUser.Username, err)
		return
	}

	// Generate insult using the generateInsult function
	insult := GenerateInsult(targetUser.Username, workoutsThisWeek, daysSinceLastWorkout)

	// 3. Update the "Thinking" message with the final insult
	_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &insult,
	})
	if err != nil {
		log.Printf("Error editing response: %v", err)
	}

	//sendResponse(s, i, insult)
}

func handleGymLog(s *discordgo.Session, i *discordgo.InteractionCreate) {
	user := i.Member.User
	currentYear := time.Now().Year()

	// Use context with timeout for better control
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use a transaction for both operations
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		sendResponse(s, i, "❌ Failed to log workout.")
		log.Printf("Error starting transaction: %v", err)
		return
	}
	defer tx.Rollback() // Will be no-op if committed

	// Insert workout with retry logic
	err = executeWithRetry(ctx, func() error {
		_, err := tx.ExecContext(ctx, "INSERT INTO workouts (user_id, username) VALUES (?, ?)", user.ID, user.Username)
		return err
	}, 3)

	if err != nil {
		sendResponse(s, i, "❌ Failed to log workout.")
		log.Printf("Error inserting workout for user %s: %v", user.Username, err)
		return
	}

	// Count total for the user in the current year in the same transaction
	var total int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) 
		FROM workouts 
		WHERE user_id = ? AND strftime('%Y', timestamp) = strftime('%Y', 'now')
	`, user.ID).Scan(&total)
	if err != nil {
		sendResponse(s, i, "❌ Failed to retrieve count.")
		log.Printf("Error counting workouts for user %s: %v", user.Username, err)
		return
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		sendResponse(s, i, "❌ Failed to log workout.")
		log.Printf("Error committing transaction: %v", err)
		return
	}

	sendResponse(s, i, fmt.Sprintf("🏋️ **%s**, workout logged! You've gone **%d** times in %d.", user.Username, total, currentYear))
}

func handleLeaderboard(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	currentYear := time.Now().Year()

	rows, err := db.QueryContext(ctx, `
		SELECT username, COUNT(*) as total 
		FROM workouts 
		WHERE strftime('%Y', timestamp) = strftime('%Y', 'now')
		GROUP BY user_id 
		ORDER BY total DESC 
		LIMIT 10`)
	if err != nil {
		sendResponse(s, i, "❌ Could not retrieve leaderboard.")
		log.Printf("Error querying leaderboard: %v", err)
		return
	}
	defer rows.Close()

	leaderboardText := ""
	rank := 1
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}
		leaderboardText += fmt.Sprintf("%d. **%s** — %d sessions\n", rank, name, count)
		rank++
	}

	if leaderboardText == "" {
		leaderboardText = "No workouts logged yet. Be the first!"
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{
				{
					Title:       fmt.Sprintf("🏆 %d Gym Leaderboard", currentYear),
					Description: leaderboardText,
					Color:       0x00ff00, // Green
				},
			},
		},
	})
}

func sendResponse(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: msg},
	})
}

// Retry helper for handling transient database lock errors
func executeWithRetry(ctx context.Context, fn func() error, maxRetries int) error {
	var err error
	for i := 0; i < maxRetries; i++ {
		err = fn()
		if err == nil {
			return nil
		}

		// Check if it's a retryable error (database locked)
		if err.Error() == "database is locked" || err.Error() == "database table is locked" {
			// Exponential backoff
			backoff := time.Duration(i+1) * 50 * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
				continue
			}
		}

		// Non-retryable error, return immediately
		return err
	}
	return fmt.Errorf("max retries exceeded: %w", err)
}

func initializeDb(db *sql.DB) error {
	var err error
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS workouts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id TEXT NOT NULL,
		username TEXT NOT NULL,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return err
	}

	// Create index for faster queries
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_user_id ON workouts(user_id)`)
	if err != nil {
		return err
	}

	// Additional performance pragmas
	pragmas := []string{
		"PRAGMA synchronous = NORMAL",  // Faster writes, still safe with WAL
		"PRAGMA cache_size = -64000",   // 64MB cache
		"PRAGMA temp_store = MEMORY",   // Keep temp tables in memory
		"PRAGMA mmap_size = 268435456", // 256MB memory-mapped I/O
	}

	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			log.Printf("Warning: could not set %s: %v", pragma, err)
		}
	}

	// Optimized connection pool for write-heavy workload
	db.SetMaxOpenConns(10) // Reduced from 25 to minimize contention
	db.SetMaxIdleConns(5)  // Keep connections ready
	db.SetConnMaxLifetime(5 * time.Minute)

	return nil
}

type OllamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type OllamaResponse struct {
	Response string `json:"response"`
}

func GenerateInsult(username string, sessionsThisWeek int, daysSinceLastSession int) string {
	url := "http://localhost:11434/api/generate"

	// The "Context" is built here
	prompt := fmt.Sprintf(`
        You are a funny, sarcastic, and punny gym bro. Like the Gordan Ramsay of the fitness world. 
        Your job is to roast users who are slacking.
        User: %s
        Days since last workout: %d
        Total workouts this week: %d

        Write a 1-sentence devastating roast based on any of the above stats or not but just make it funny to the others. 
        Be creative, use gym slang, and don't be generic.`,
		username, daysSinceLastSession, sessionsThisWeek)

	payload := OllamaRequest{
		Model:  "llama3",
		Prompt: prompt,
		Stream: false,
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Sprintf("%s is so lazy even the AI gave up on roasting them.", username)
	}
	defer resp.Body.Close()

	var ollamaResp OllamaResponse
	json.NewDecoder(resp.Body).Decode(&ollamaResp)
	return cleanInsult(ollamaResp.Response)
}

func cleanInsult(raw string) string {
	// 1. Unquote the string if it's double-encoded (removes \" and outer quotes)
	unquoted, err := strconv.Unquote(raw)
	if err != nil {
		// If it fails, it wasn't double-quoted, so just use the original
		unquoted = raw
	}

	// 2. Manually trim any remaining literal quote marks that the AI might have added
	return strings.Trim(unquoted, "\" ")
}
