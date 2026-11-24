// Command sendtips sends the "invite tips" marketing email to a hand-picked
// list of recipients who have agreed to receive it. It is a stop-gap until
// opt-in is stored per user. It defaults to a dry run; pass -send to deliver.
//
// Recipients file: one per line, "email" or "email,First name". Blank lines
// and lines starting with # are ignored. SMTP settings come from SMTP_ADDR,
// SMTP_USER, SMTP_PASS and MAIL_FROM, the same variables the API uses.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net/mail"
	"os"
	"strings"
	"time"

	mailer "github.com/ansonarose/seeyouthere-backend/internal/mail"
)

const maxRecipients = 200 // stays well under Zoho's daily sending limit

type recipient struct {
	email string
	name  string
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("sendtips failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		recipientsPath = flag.String("recipients", "", "recipients file (required)")
		htmlPath       = flag.String("html", "", "HTML template (required)")
		textPath       = flag.String("text", "", "plain-text template (required)")
		subject        = flag.String("subject", "Your next invite deserves a better first impression", "email subject")
		createURL      = flag.String("create-url", "https://seeyouthere.at/create", "link for the call-to-action button")
		editURL        = flag.String("edit-url", "https://seeyouthere.at/app", "link to the recipient's invite dashboard")
		unsubAddr      = flag.String("unsubscribe", "", "address that receives unsubscribe requests (required)")
		delay          = flag.Duration("delay", 2*time.Second, "pause between messages")
		send           = flag.Bool("send", false, "actually send; without it only validates and lists counts")
	)
	flag.Parse()

	if *recipientsPath == "" || *htmlPath == "" || *textPath == "" || *unsubAddr == "" {
		flag.Usage()
		return errors.New("-recipients, -html, -text and -unsubscribe are required")
	}
	if _, err := mail.ParseAddress(*unsubAddr); err != nil {
		return errors.New("-unsubscribe is not a valid address")
	}

	recipients, err := readRecipients(*recipientsPath)
	if err != nil {
		return err
	}
	htmlTpl, err := os.ReadFile(*htmlPath)
	if err != nil {
		return fmt.Errorf("read html template: %w", err)
	}
	textTpl, err := os.ReadFile(*textPath)
	if err != nil {
		return fmt.Errorf("read text template: %w", err)
	}

	slog.Info("recipients loaded", "count", len(recipients), "send", *send)
	if !*send {
		slog.Info("dry run only; pass -send to deliver")
		return nil
	}

	addr := os.Getenv("SMTP_ADDR")
	if addr == "" {
		return errors.New("SMTP_ADDR is required")
	}
	from := os.Getenv("MAIL_FROM")
	if from == "" {
		from = "See You There <hello@seeyouthere.at>"
	}
	sender := &mailer.SMTPSender{Addr: addr, User: os.Getenv("SMTP_USER"), Pass: os.Getenv("SMTP_PASS"), From: from}

	unsubMailto := "mailto:" + *unsubAddr + "?subject=unsubscribe"
	ctx := context.Background()
	failed := 0
	for i, r := range recipients {
		name := r.name
		if name == "" {
			name = "there"
		}
		htmlBody := strings.NewReplacer(
			"{{first_name}}", html.EscapeString(name),
			"{{create_url}}", html.EscapeString(*createURL),
			"{{edit_url}}", html.EscapeString(*editURL),
		).Replace(string(htmlTpl))
		textBody := strings.NewReplacer(
			"{{first_name}}", name,
			"{{create_url}}", *createURL,
			"{{edit_url}}", *editURL,
		).Replace(string(textTpl))

		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := sender.Send(sendCtx, mailer.Message{
			To:              r.email,
			Subject:         *subject,
			Text:            textBody,
			HTML:            htmlBody,
			ListUnsubscribe: "<" + unsubMailto + ">",
		})
		cancel()
		if err != nil {
			failed++
			// Index, not address: logs must not contain recipient emails.
			slog.Error("send failed", "recipient_index", i+1, "error", err)
		} else {
			slog.Info("sent", "recipient_index", i+1, "of", len(recipients))
		}
		if i < len(recipients)-1 {
			time.Sleep(*delay)
		}
	}
	slog.Info("done", "sent", len(recipients)-failed, "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d sends failed", failed, len(recipients))
	}
	return nil
}

// readRecipients parses and de-duplicates the recipients file.
func readRecipients(path string) ([]recipient, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open recipients: %w", err)
	}
	defer f.Close()

	seen := map[string]bool{}
	var out []recipient
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		email, name, _ := strings.Cut(s, ",")
		email = strings.ToLower(strings.TrimSpace(email))
		name = strings.TrimSpace(name)
		a, err := mail.ParseAddress(email)
		if err != nil || a.Address != email {
			return nil, fmt.Errorf("recipients line %d: invalid email", line)
		}
		if seen[email] {
			continue
		}
		seen[email] = true
		out = append(out, recipient{email: email, name: name})
		if len(out) > maxRecipients {
			return nil, fmt.Errorf("more than %d recipients; split the list", maxRecipients)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read recipients: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("recipients file has no addresses")
	}
	return out, nil
}
