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
		{
			Name:        "compliment",
			Description: "Get a gym-related compliment",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "user",
					Description: "The user you want to praise",
					Required:    true,
				},
			},
		},
		{
			Name:        "stats",
			Description: "View your gym statistics",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "user",
					Description: "The user whose stats you want to see (optional)",
					Required:    false,
				},
			},
		},
		{
			Name:        "streaks",
			Description: "Show the top current streaks",
		},
	}

	for _, v := range commands {
		_, err := dg.ApplicationCommandCreate(dg.State.User.ID, "", v)
		if err != nil {
			log.Panicf("Cannot create '%v' command: %v", v.Name, err)
		}
	}

	fmt.Println("Gym Bot is active. Press CTRL+C to stop.")

	// Send a startup message to the gym channel
	sendStartupMessage(dg, gymChannelID)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop

	// Send a shutdown message to the gym channel
	sendShutdownMessage(dg, gymChannelID)
}

// sendStartupMessage sends a message to the Discord channel when the bot starts up
func sendStartupMessage(s *discordgo.Session, channelID string) {
	message := "💪 Gym Bot is online and ready to pump some iron! Use `/gym` to log your sessions!"
	_, err := s.ChannelMessageSend(channelID, message)
	if err != nil {
		log.Printf("Error sending startup message: %v", err)
	}
}

// sendShutdownMessage sends a message to the Discord channel when the bot shuts down
func sendShutdownMessage(s *discordgo.Session, channelID string) {
	message := "👋 Gym Bot is shutting down. See you at the gym next time!"
	_, err := s.ChannelMessageSend(channelID, message)
	if err != nil {
		log.Printf("Error sending shutdown message: %v", err)
	}
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
	case "compliment":
		handleCompliment(s, i)
	case "stats":
		handleStats(s, i)
	case "streaks":
		handleStreaks(s, i)
	}
}

func handleCompliment(s *discordgo.Session, i *discordgo.InteractionCreate) {
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
	insult := GenerateCompliment(targetUser.Username, workoutsThisWeek, daysSinceLastWorkout)

	// 3. Update the "Thinking" message with the final insult
	_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &insult,
	})
	if err != nil {
		log.Printf("Error editing response: %v", err)
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

	// Calculate workouts this week after committing
	var workoutsThisWeek int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) 
		FROM workouts 
		WHERE user_id = ? AND strftime('%W', timestamp) = strftime('%W', 'now')
	`, user.ID).Scan(&workoutsThisWeek)
	if err != nil {
		log.Printf("Error counting weekly workouts for user %s: %v", user.Username, err)
		workoutsThisWeek = -1 // Default to -1 (indicating an error)
	}

	// Build response message
	message := fmt.Sprintf("🏋️ **%s**, workout logged! You've gone **%d** times in %d.", user.Username, total, currentYear)
	message += fmt.Sprintf(" 💪 **%d** workouts this week!", workoutsThisWeek)

	sendResponse(s, i, message)
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

		emoji := ""
		if rank == 1 {
			emoji = "🥇"
		} else if rank == 2 {
			emoji = "🥈"
		} else if rank == 3 {
			emoji = "🥉"
		}

		leaderboardText += fmt.Sprintf("%s **%s** — %d sessions\n", emoji, name, count)
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

func handleStats(s *discordgo.Session, i *discordgo.InteractionCreate) {
	options := i.ApplicationCommandData().Options
	var targetUser *discordgo.User

	if len(options) > 0 && options[0].Name == "user" {
		targetUser = options[0].UserValue(s)
	} else {
		targetUser = i.Member.User
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Get comprehensive stats
	var totalWorkouts, workoutsThisYear, workoutsThisMonth, workoutsThisWeek int
	var currentStreak, longestStreak int
	var lastWorkout sql.NullString

	err := db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*) as total,
			SUM(CASE WHEN strftime('%Y', timestamp) = strftime('%Y', 'now') THEN 1 ELSE 0 END) as this_year,
			SUM(CASE WHEN strftime('%Y-%m', timestamp) = strftime('%Y-%m', 'now') THEN 1 ELSE 0 END) as this_month,
			SUM(CASE WHEN strftime('%W', timestamp) = strftime('%W', 'now') THEN 1 ELSE 0 END) as this_week,
			MAX(timestamp) as last_workout
		FROM workouts
		WHERE user_id = ?
	`, targetUser.ID).Scan(&totalWorkouts, &workoutsThisYear, &workoutsThisMonth, &workoutsThisWeek, &lastWorkout)

	if err != nil {
		sendResponse(s, i, "❌ Could not retrieve stats.")
		log.Printf("Error querying stats: %v", err)
		return
	}

	// Calculate streaks
	currentStreak = calculateCurrentStreak(ctx, targetUser.ID)
	longestStreak = calculateLongestStreak(ctx, targetUser.ID)

	// Format last workout time
	lastWorkoutStr := "Never"
	if lastWorkout.Valid {
		t, err := time.Parse("2006-01-02 15:04:05", lastWorkout.String)
		if err == nil {
			daysSince := int(time.Since(t).Hours() / 24)
			if daysSince == 0 {
				lastWorkoutStr = "Today"
			} else if daysSince == 1 {
				lastWorkoutStr = "Yesterday"
			} else {
				lastWorkoutStr = fmt.Sprintf("%d days ago", daysSince)
			}
		}
	}

	// Create embed
	embed := &discordgo.MessageEmbed{
		Title: fmt.Sprintf("📊 Gym Stats for %s", targetUser.Username),
		Color: 0x3498db, // Blue
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "🔥 Current Streak",
				Value:  fmt.Sprintf("%d days", currentStreak),
				Inline: true,
			},
			{
				Name:   "🏆 Longest Streak",
				Value:  fmt.Sprintf("%d days", longestStreak),
				Inline: true,
			},
			{
				Name:   "⏱️ Last Workout",
				Value:  lastWorkoutStr,
				Inline: true,
			},
			{
				Name:   "📅 This Week",
				Value:  fmt.Sprintf("%d workouts", workoutsThisWeek),
				Inline: true,
			},
			{
				Name:   "📆 This Month",
				Value:  fmt.Sprintf("%d workouts", workoutsThisMonth),
				Inline: true,
			},
			{
				Name:   "📈 This Year",
				Value:  fmt.Sprintf("%d workouts", workoutsThisYear),
				Inline: true,
			},
			{
				Name:   "💪 All Time",
				Value:  fmt.Sprintf("%d workouts", totalWorkouts),
				Inline: false,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

func handleStreaks(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Get all users with workouts
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT user_id, username
		FROM workouts
	`)
	if err != nil {
		sendResponse(s, i, "❌ Could not retrieve streaks.")
		log.Printf("Error querying users: %v", err)
		return
	}
	defer rows.Close()

	type StreakInfo struct {
		Username      string
		CurrentStreak int
		LongestStreak int
	}

	var streaks []StreakInfo
	for rows.Next() {
		var userID, username string
		if err := rows.Scan(&userID, &username); err != nil {
			continue
		}

		currentStreak := calculateCurrentStreak(ctx, userID)
		longestStreak := calculateLongestStreak(ctx, userID)

		streaks = append(streaks, StreakInfo{
			Username:      username,
			CurrentStreak: currentStreak,
			LongestStreak: longestStreak,
		})
	}

	if len(streaks) == 0 {
		sendResponse(s, i, "No workouts logged yet!")
		return
	}

	// Sort by current streak (bubble sort for simplicity)
	for i := 0; i < len(streaks)-1; i++ {
		for j := 0; j < len(streaks)-i-1; j++ {
			if streaks[j].CurrentStreak < streaks[j+1].CurrentStreak {
				streaks[j], streaks[j+1] = streaks[j+1], streaks[j]
			}
		}
	}

	// Build leaderboard text
	currentStreakText := ""
	longestStreakText := ""

	for idx, streak := range streaks {
		if idx < 10 { // Top 10 for current streaks
			emoji := ""
			if idx == 0 {
				emoji = "🥇"
			} else if idx == 1 {
				emoji = "🥈"
			} else if idx == 2 {
				emoji = "🥉"
			}
			currentStreakText += fmt.Sprintf("%s **%s** — %d days\n", emoji, streak.Username, streak.CurrentStreak)
		}
	}

	// Sort by longest streak for second section
	for i := 0; i < len(streaks)-1; i++ {
		for j := 0; j < len(streaks)-i-1; j++ {
			if streaks[j].LongestStreak < streaks[j+1].LongestStreak {
				streaks[j], streaks[j+1] = streaks[j+1], streaks[j]
			}
		}
	}

	for idx, streak := range streaks {
		if idx < 5 { // Top 5 for longest streaks
			longestStreakText += fmt.Sprintf("**%s** — %d days\n", streak.Username, streak.LongestStreak)
		}
	}

	embed := &discordgo.MessageEmbed{
		Title: "🔥 Gym Streaks",
		Color: 0xff6b6b, // Red-orange
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "Current Streaks",
				Value:  currentStreakText,
				Inline: false,
			},
			{
				Name:   "All-Time Longest Streaks",
				Value:  longestStreakText,
				Inline: false,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

func calculateCurrentStreak(ctx context.Context, userID string) int {
	// Get all workout dates for this user, ordered by date descending
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT DATE(timestamp) as workout_date
		FROM workouts
		WHERE user_id = ?
		ORDER BY workout_date DESC
	`, userID)
	if err != nil {
		log.Printf("Error calculating current streak: %v", err)
		return 0
	}
	defer rows.Close()

	var dates []time.Time
	for rows.Next() {
		var dateStr string
		if err := rows.Scan(&dateStr); err != nil {
			continue
		}
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		dates = append(dates, t)
	}

	if len(dates) == 0 {
		return 0
	}

	today := time.Now().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)

	// Streak must start with today or yesterday
	if !dates[0].Equal(today) && !dates[0].Equal(yesterday) {
		return 0
	}

	// Count consecutive days
	streak := 1
	for i := 1; i < len(dates); i++ {
		daysDiff := int(dates[i-1].Sub(dates[i]).Hours() / 24)
		if daysDiff == 1 {
			streak++
		} else {
			break
		}
	}

	return streak
}

func calculateLongestStreak(ctx context.Context, userID string) int {
	// Get all workout dates for this user, ordered by date ascending
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT DATE(timestamp) as workout_date
		FROM workouts
		WHERE user_id = ?
		ORDER BY workout_date ASC
	`, userID)
	if err != nil {
		log.Printf("Error calculating longest streak: %v", err)
		return 0
	}
	defer rows.Close()

	var dates []time.Time
	for rows.Next() {
		var dateStr string
		if err := rows.Scan(&dateStr); err != nil {
			continue
		}
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		dates = append(dates, t)
	}

	if len(dates) == 0 {
		return 0
	}

	maxStreak := 1
	currentStreak := 1

	for i := 1; i < len(dates); i++ {
		daysDiff := int(dates[i].Sub(dates[i-1]).Hours() / 24)

		if daysDiff == 1 {
			currentStreak++
			if currentStreak > maxStreak {
				maxStreak = currentStreak
			}
		} else {
			currentStreak = 1
		}
	}

	return maxStreak
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

func GenerateCompliment(username string, sessionsThisWeek int, daysSinceLastSession int) string {
	url := "http://localhost:11434/api/generate"

	// The "Context" is built here
	prompt := fmt.Sprintf(`
		You are a funny, uplifting, and punny gym bro. Like the Gordan Ramsay of the fitness world.
		Your job is to compliment users who are doing great.
		User: %s
		Days since last workout: %d
		Total workouts this week: %d

		Write a 1-sentence hilarious compliment based on any of the above stats or not but just make it funny to the others.
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
		return fmt.Sprintf("%s is so awesome even the AI couldn't help but praise them.", username)
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
