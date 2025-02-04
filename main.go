package main

import (
	"crypto/tls"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/smtp"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

const (
	emailDelay    = 2 * time.Second
	emailsPerHour = 180
	hourDuration  = time.Hour
)

type SMTPConfig struct {
	Host     string `json:"smtp_host"`
	Port     int    `json:"smtp_port"`
	User     string `json:"smtp_user"`
	Password string `json:"smtp_pass"`
	From     string `json:"from"`
}

type Recipient struct {
	SenderName     string `json:"SenderName"`
	RecipientName  string `json:"RecipientName"`
	RecipientEmail string `json:"RecipientEmail"`
}

var (
	smtpConfig    SMTPConfig
	recipients    []Recipient
	emailSubject  string
	emailBody     string
	db            *sql.DB
	progressBar   *widget.ProgressBar
	resultLabel   *widget.Label
	statusLabel   *widget.Label // Add a status label
	reportContent string
	successCount  int
	failureCount  int
	totalEmails   int
)

func main() {
	// Initialize SQLite
	var err error
	db, err = sql.Open("sqlite3", "./email_queue.db")
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}
	defer db.Close()

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS email_queue (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            sender_name TEXT,
            recipient_name TEXT,
            recipient_email TEXT,
            subject TEXT,
            body TEXT,
            status TEXT DEFAULT 'pending'
        )
    `)
	if err != nil {
		log.Fatal("Failed to create table:", err)
	}

	// Clean up the database
	_, err = db.Exec(`DELETE FROM email_queue`)
	if err != nil {
		log.Fatal("Failed to clean up email queue:", err)
	}

	// Initialize Fyne app
	myApp := app.New()
	myWindow := myApp.NewWindow("Neon Mails")

	// Widgets
	smtpButton := widget.NewButton("Load SMTP Config", func() { loadSMTPConfig(myWindow) })
	recipientButton := widget.NewButton("Load Recipients", func() { loadRecipients(myWindow) })
	templateButton := widget.NewButton("Load Template", func() { loadTemplate(myWindow) })
	queueButton := widget.NewButton("Queue Emails", queueEmails)
	sendButton := widget.NewButton("Send Emails", sendEmails)

	progressBar = widget.NewProgressBar()
	resultLabel = widget.NewLabel("")
	statusLabel = widget.NewLabel("") // Initialize the status label

	// Layout
	content := container.NewVBox(
		smtpButton,
		recipientButton,
		templateButton,
		queueButton,
		sendButton,
		progressBar,
		resultLabel,
		statusLabel, // Add the status label to the UI
	)

	myWindow.SetContent(content)
	myWindow.Resize(fyne.NewSize(400, 400))
	myWindow.ShowAndRun()
}

func loadSMTPConfig(win fyne.Window) {
	dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
		if err != nil || reader == nil {
			statusLabel.SetText("Failed to open file.")
			return
		}
		defer reader.Close()

		content, err := ioutil.ReadAll(reader)
		if err != nil {
			statusLabel.SetText("Failed to read file.")
			return
		}

		err = json.Unmarshal(content, &smtpConfig)
		if err != nil {
			statusLabel.SetText("Invalid SMTP config file.")
			return
		}

		statusLabel.SetText("SMTP config loaded successfully.")
	}, win).Show()
}

func loadRecipients(win fyne.Window) {
	dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
		if err != nil || reader == nil {
			statusLabel.SetText("Failed to open file.")
			return
		}
		defer reader.Close()

		csvReader := csv.NewReader(reader)
		records, err := csvReader.ReadAll()
		if err != nil {
			statusLabel.SetText("Failed to read CSV file.")
			return
		}

		// Skip the header row
		if len(records) > 0 {
			records = records[1:]
		}

		recipients = make([]Recipient, len(records))
		for i, record := range records {
			if len(record) != 3 {
				statusLabel.SetText("Invalid CSV format.")
				return
			}

			recipients[i] = Recipient{
				SenderName:     record[0],
				RecipientName:  record[1],
				RecipientEmail: record[2],
			}
		}

		statusLabel.SetText(fmt.Sprintf("Recipients loaded: %d", len(recipients)))
	}, win).Show()
}

func loadTemplate(win fyne.Window) {
	dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
		if err != nil || reader == nil {
			statusLabel.SetText("Failed to open file.")
			return
		}
		defer reader.Close()

		content, err := ioutil.ReadAll(reader)
		if err != nil {
			statusLabel.SetText("Failed to read file.")
			return
		}

		lines := strings.Split(string(content), "\n")
		subject := ""
		body := ""

		for i, line := range lines {
			if i == 0 {
				subject = strings.TrimPrefix(line, "Subject: ")
			} else {
				body += line + "\n"
			}
		}

		emailSubject = subject
		emailBody = body

		statusLabel.SetText("Template loaded successfully.")
	}, win).Show()
}

func queueEmails() {
	for _, recipient := range recipients {
		_, err := db.Exec(`
            INSERT INTO email_queue (sender_name, recipient_name, recipient_email, subject, body)
            VALUES (?, ?, ?, ?, ?)
        `, recipient.SenderName, recipient.RecipientName, recipient.RecipientEmail, emailSubject, emailBody)
		if err != nil {
			log.Println("Failed to queue email:", err)
		}
	}
	resultLabel.SetText("Emails queued successfully.")
}

func sendEmails() {
	// Fetch pending emails from the database
	rows, err := db.Query(`SELECT id, sender_name, recipient_name, recipient_email, subject, body FROM email_queue WHERE status = 'pending'`)
	if err != nil {
		log.Println("Failed to fetch queue:", err)
		resultLabel.SetText("Failed to fetch email queue.")
		return
	}
	defer rows.Close()

	// Count the total number of pending emails
	err = db.QueryRow(`SELECT COUNT(*) FROM email_queue WHERE status = 'pending'`).Scan(&totalEmails)
	if err != nil {
		log.Println("Failed to count pending emails:", err)
		resultLabel.SetText("Failed to count pending emails.")
		return
	}

	if totalEmails == 0 {
		resultLabel.SetText("No pending emails to send.")
		return
	}

	var wg sync.WaitGroup

	// Channel to send progress updates
	updateChan := make(chan struct{})

	// Goroutine to listen for updates and update the UI
	go func() {
		for range updateChan {
			progress := float64(successCount+failureCount) / float64(totalEmails)
			progressBar.SetValue(progress)
			resultLabel.SetText(fmt.Sprintf("Progress: %.0f%%, Sent: %d, Failed: %d", progress*100, successCount, failureCount))
		}
	}()

	// Increase the number of concurrent goroutines
	concurrency := 10
	sem := make(chan struct{}, concurrency)

	for rows.Next() {
		var id int
		var senderName, recipientName, recipientEmail, subject, body string

		err = rows.Scan(&id, &senderName, &recipientName, &recipientEmail, &subject, &body)
		if err != nil {
			log.Println("Failed to scan row:", err)
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(id int, senderName, recipientName, recipientEmail, subject, body string) {
			defer wg.Done()
			defer func() { <-sem }()

			err := sendEmail(smtpConfig, recipientEmail, recipientName, senderName, subject, body)
			if err != nil {
				log.Printf("Failed to send email to %s: %v\n", recipientEmail, err)
				failureCount++
			} else {
				log.Printf("Email sent to %s successfully\n", recipientEmail)
				successCount++
			}

			// Update the email queue status
			_, err = db.Exec(`UPDATE email_queue SET status = ? WHERE id = ?`, "sent", id)
			if err != nil {
				log.Println("Failed to update email queue status:", err)
			}

			// Send progress update to the channel
			updateChan <- struct{}{}
			time.Sleep(emailDelay)
		}(id, senderName, recipientName, recipientEmail, subject, body)
	}

	wg.Wait()

	// Clear the database after all emails are processed
	_, err = db.Exec(`DELETE FROM email_queue`)
	if err != nil {
		log.Println("Failed to clear email queue:", err)
	}

	// Final progress bar update and report
	progressBar.SetValue(1.0) // Ensure progress bar reaches 100%
	resultLabel.SetText(fmt.Sprintf("Job completed! Emails sent: %d, Failed: %d", successCount, failureCount))

	log.Printf("Job completed! Emails sent: %d, Failed: %d\n", successCount, failureCount)
}

func sendEmail(smtpConfig SMTPConfig, recipientEmail, recipientName, senderName, subject, body string) error {
	subject = strings.ReplaceAll(subject, "{name}", recipientName)
	subject = strings.ReplaceAll(subject, "{sender}", senderName)
	body = strings.ReplaceAll(body, "{name}", recipientName)
	body = strings.ReplaceAll(body, "{sender}", senderName)

	// Set the sender's name and email address
	sender := fmt.Sprintf("%s <%s>", senderName, smtpConfig.From)

	auth := smtp.PlainAuth("", smtpConfig.User, smtpConfig.Password, smtpConfig.Host)
	msg := []byte(fmt.Sprintf("From: %s\nSubject: %s\n\n%s", sender, subject, body))

	// Retry logic
	for i := 0; i < 3; i++ {
		// Establish a connection to the SMTP server
		c, err := smtp.Dial(fmt.Sprintf("%s:%d", smtpConfig.Host, smtpConfig.Port))
		if err != nil {
			log.Printf("Failed to connect to SMTP server: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		defer c.Close()

		// Upgrade the connection to a secure one if necessary
		if smtpConfig.Port == 587 {
			tlsConfig := &tls.Config{
				ServerName: smtpConfig.Host,
			}
			if err := c.StartTLS(tlsConfig); err != nil {
				log.Printf("Failed to start TLS: %v", err)
				time.Sleep(2 * time.Second)
				continue
			}
		}

		// Authenticate with the SMTP server
		if err := c.Auth(auth); err != nil {
			log.Printf("Failed to authenticate: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Set the sender and recipient
		if err := c.Mail(smtpConfig.From); err != nil {
			log.Printf("Failed to set sender: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if err := c.Rcpt(recipientEmail); err != nil {
			log.Printf("Failed to set recipient: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Send the email body
		wc, err := c.Data()
		if err != nil {
			log.Printf("Failed to send data: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		defer wc.Close()
		if _, err = wc.Write(msg); err != nil {
			log.Printf("Failed to write message: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// log.Printf("Email sent to %s successfully", recipientEmail)
		return nil
	}

	return fmt.Errorf("failed to send email to %s after multiple attempts", recipientEmail)
}
