package main

import (
	"bufio"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// These defaults keep the demo easy to run locally while still feeling like
// a real library systems dashboard.
const (
	defaultUserGroup = "STAFF"
	refreshInterval  = 60 * time.Second
	pageSize         = 100
)

// Loan is the presentation-friendly shape we render in the terminal.
// Alma gives us much more data, but this is the slice we care about on stage.
type Loan struct {
	Borrower       string
	Title          string
	Barcode        string
	DueDate        time.Time
	DueDateDisplay string
	Status         string
	ProcessStatus  string
}

type sortMode int

const (
	sortByUser sortMode = iota
	sortByTitle
	sortByDueDate
	sortByStatus
)

func (s sortMode) Label() string {
	switch s {
	case sortByUser:
		return "User"
	case sortByTitle:
		return "Title"
	case sortByStatus:
		return "Status"
	default:
		return "Due Date"
	}
}

type filterMode int

const (
	filterAll filterMode = iota
	filterOverdue
	filterLost
	filterClaims
)

func (f filterMode) Label() string {
	switch f {
	case filterOverdue:
		return "Overdue"
	case filterLost:
		return "Lost"
	case filterClaims:
		return "Claims"
	default:
		return "All"
	}
}

type almaConfig struct {
	BaseURL   string
	APIKey    string
	UserGroup string
}

// model holds the live state of the whole app: the data, the UI controls,
// and the little bits of feedback we show to the user.
type model struct {
	table         table.Model
	loans         []Loan
	loading       bool
	refreshing    bool
	spinner       spinner.Model
	searchInput   textinput.Model
	searchMode    bool
	searchQuery   string
	statusMessage string
	err           error
	sortMode      sortMode
	filterMode    filterMode
	lastUpdated   time.Time
	visibleCount  int
	config        almaConfig
}

type almaUsersResponse struct {
	XMLName          xml.Name   `xml:"users"`
	TotalRecordCount int        `xml:"total_record_count,attr"`
	Users            []almaUser `xml:"user"`
}

type almaUser struct {
	PrimaryID string `xml:"primary_id"`
}

type almaItemLoansResponse struct {
	XMLName          xml.Name       `xml:"item_loans"`
	TotalRecordCount int            `xml:"total_record_count,attr"`
	Loans            []almaItemLoan `xml:"item_loan"`
}

type almaItemLoan struct {
	UserID        string `xml:"user_id"`
	ItemBarcode   string `xml:"item_barcode"`
	DueDate       string `xml:"due_date"`
	LoanStatus    string `xml:"loan_status"`
	ProcessStatus string `xml:"process_status"`
	Title         string `xml:"title"`
}

type dataLoadedMsg struct {
	loans     []Loan
	err       error
	fetchedAt time.Time
}

type refreshMsg time.Time

// loadData kicks off a background fetch so the UI stays responsive while Alma
// does its work.
func loadData(cfg almaConfig) tea.Cmd {
	return func() tea.Msg {
		loans, err := fetchLoansFromAlma(cfg)
		return dataLoadedMsg{loans: loans, err: err, fetchedAt: time.Now()}
	}
}

// scheduleRefresh gives the dashboard a steady heartbeat so the numbers stay fresh.
func scheduleRefresh() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return refreshMsg(t)
	})
}

// initialModel sets up the table and search box so the app feels ready the
// moment it opens, even before the first API call finishes.
func initialModel() model {
	cfg := loadConfig()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	columns := []table.Column{
		{Title: "User", Width: 18},
		{Title: "Title", Width: 42},
		{Title: "Barcode", Width: 14},
		{Title: "Due Date", Width: 12},
		{Title: "Status", Width: 14},
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(12),
	)

	styles := table.DefaultStyles()
	styles.Selected = styles.Selected.
		Foreground(lipgloss.Color("230")).
		Background(lipgloss.Color("62")).
		Bold(true)
	t.SetStyles(styles)

	ti := textinput.New()
	ti.Placeholder = "Search user, title, barcode, or status"
	ti.Prompt = "Search: "
	ti.Width = 48

	return model{
		table:       t,
		loading:     true,
		spinner:     s,
		searchInput: ti,
		sortMode:    sortByDueDate,
		filterMode:  filterAll,
		config:      cfg,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, loadData(m.config), scheduleRefresh())
}

// currentVisibleLoans returns the exact slice the audience is looking at after
// filtering, searching, and sorting have all been applied.
func (m *model) currentVisibleLoans() []Loan {
	filtered := filterLoans(m.loans, m.filterMode)
	filtered = searchLoans(filtered, m.searchQuery)
	sortLoans(filtered, m.sortMode)
	return filtered
}

// applyTable is where raw Alma data becomes the on-screen table.
func (m *model) applyTable() {
	filtered := m.currentVisibleLoans()
	m.visibleCount = len(filtered)

	rows := make([]table.Row, 0, len(filtered))
	for _, l := range filtered {
		rows = append(rows, table.Row{l.Borrower, l.Title, l.Barcode, l.DueDateDisplay, l.Status})
	}

	if len(rows) == 0 {
		rows = []table.Row{{"-", "No matching loans found", "-", "-", "-"}}
	}

	m.table.SetRows(rows)
}

// Update is the dashboard's traffic controller. It reacts to keyboard input,
// refresh ticks, and incoming Alma data.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.searchMode {
			switch msg.String() {
			case "esc":
				m.searchMode = false
				m.searchInput.Blur()
				return m, nil
			case "enter":
				m.searchMode = false
				m.searchQuery = strings.TrimSpace(m.searchInput.Value())
				m.searchInput.Blur()
				m.applyTable()
				return m, nil
			}

			m.searchInput, cmd = m.searchInput.Update(msg)
			m.searchQuery = strings.TrimSpace(m.searchInput.Value())
			m.applyTable()
			return m, cmd
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "/":
			m.searchMode = true
			m.searchInput.SetValue(m.searchQuery)
			return m, m.searchInput.Focus()
		case "ctrl+l":
			m.searchQuery = ""
			m.searchInput.SetValue("")
			m.statusMessage = "Search cleared"
			m.applyTable()
			return m, nil
		case "e":
			fileName := fmt.Sprintf("alma_%s_loans_%s.csv", strings.ToLower(m.config.UserGroup), time.Now().Format("20060102_150405"))
			if err := exportLoansToCSV(m.currentVisibleLoans(), fileName); err != nil {
				m.statusMessage = "Export failed: " + err.Error()
			} else {
				m.statusMessage = "Exported CSV: " + fileName
			}
			return m, nil
		case "r":
			m.refreshing = true
			m.err = nil
			return m, loadData(m.config)
		case "s":
			m.sortMode = (m.sortMode + 1) % 4
			m.applyTable()
			return m, nil
		case "f":
			m.filterMode = (m.filterMode + 1) % 4
			m.applyTable()
			return m, nil
		case "1":
			m.sortMode = sortByUser
			m.applyTable()
			return m, nil
		case "2":
			m.sortMode = sortByTitle
			m.applyTable()
			return m, nil
		case "3":
			m.sortMode = sortByDueDate
			m.applyTable()
			return m, nil
		case "4":
			m.sortMode = sortByStatus
			m.applyTable()
			return m, nil
		}

	case spinner.TickMsg:
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case refreshMsg:
		m.refreshing = true
		return m, tea.Batch(loadData(m.config), scheduleRefresh())

	case dataLoadedMsg:
		m.loading = false
		m.refreshing = false
		m.err = msg.err
		if msg.err == nil {
			m.loans = msg.loans
			m.lastUpdated = msg.fetchedAt
			m.applyTable()
		}
		return m, nil
	}

	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// View turns the current state into something clean, readable, and demo-friendly.
var (
	baseStyle   = lipgloss.NewStyle().BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240"))
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	mutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
)

func (m model) View() string {
	if m.loading {
		return fmt.Sprintf("\n  %s Fetching live Alma loan data for user group %s...\n\n", m.spinner.View(), m.config.UserGroup)
	}

	header := headerStyle.Render("Alma Fulfillment Pulse")
	summary := fmt.Sprintf("User Group: %s • Showing %d of %d loans • Sort: %s • Filter: %s", m.config.UserGroup, m.visibleCount, len(m.loans), m.sortMode.Label(), m.filterMode.Label())
	if m.searchQuery != "" {
		summary += fmt.Sprintf(" • Search: %q", m.searchQuery)
	}

	if !m.lastUpdated.IsZero() {
		summary += fmt.Sprintf(" • Updated: %s", m.lastUpdated.Format("15:04:05"))
	}
	if m.refreshing {
		summary += fmt.Sprintf(" • %s Refreshing", m.spinner.View())
	}

	errLine := ""
	if m.err != nil {
		errLine = "\n  " + errorStyle.Render(m.err.Error()) + "\n"
	}

	statusLine := ""
	if m.statusMessage != "" {
		statusLine = "\n  " + mutedStyle.Render(m.statusMessage) + "\n"
	}

	searchLine := mutedStyle.Render("Press / to search")
	if m.searchMode || m.searchQuery != "" {
		searchLine = m.searchInput.View()
	}

	footer := mutedStyle.Render("↑/↓ navigate • / search • e export • esc close • ctrl+l clear • r refresh • s sort • f filter • q quit")

	return fmt.Sprintf("\n  %s\n  %s%s%s\n  %s\n%s\n\n  %s\n", header, summary, errLine, statusLine, searchLine, baseStyle.Render(m.table.View()), footer)
}

// loadConfig reads the local environment file so the demo can stay simple:
// drop in a key, run the app, and you are live.
func loadConfig() almaConfig {
	for _, path := range []string{".env.sandbox", ".env"} {
		_ = loadEnvFile(path)
	}

	cfg := almaConfig{
		BaseURL:   strings.TrimRight(strings.TrimSpace(os.Getenv("ALMA_API_BASE_URL")), "/"),
		APIKey:    strings.TrimSpace(os.Getenv("ALMA_API_KEY")),
		UserGroup: strings.TrimSpace(os.Getenv("ALMA_USER_GROUP")),
	}
	if cfg.UserGroup == "" {
		cfg.UserGroup = defaultUserGroup
	}
	return cfg
}

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, value)
	}

	return scanner.Err()
}

// exportLoansToCSV writes the current on-screen view to a timestamped CSV.
func exportLoansToCSV(loans []Loan, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if err := writer.Write([]string{"Borrower", "Title", "Barcode", "Due Date", "Status", "Process Status"}); err != nil {
		return err
	}

	for _, loan := range loans {
		if err := writer.Write([]string{
			loan.Borrower,
			loan.Title,
			loan.Barcode,
			loan.DueDateDisplay,
			loan.Status,
			loan.ProcessStatus,
		}); err != nil {
			return err
		}
	}

	return writer.Error()
}

// fetchLoansFromAlma does the real work: find the STAFF users, pull their loans,
// and flatten everything into one list for the dashboard.
func fetchLoansFromAlma(cfg almaConfig) ([]Loan, error) {
	if cfg.BaseURL == "" || cfg.APIKey == "" {
		return nil, fmt.Errorf("missing ALMA_API_BASE_URL or ALMA_API_KEY in .env.sandbox")
	}

	client := &http.Client{Timeout: 20 * time.Second}
	users, err := fetchUsersByGroup(client, cfg)
	if err != nil {
		return nil, err
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		allLoans []Loan
		firstErr error
		sem      = make(chan struct{}, 8)
	)

	for _, user := range users {
		user := user
		if strings.TrimSpace(user.PrimaryID) == "" {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			loans, err := fetchUserLoans(client, cfg, user.PrimaryID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			allLoans = append(allLoans, loans...)
		}()
	}

	wg.Wait()

	if len(allLoans) == 0 && firstErr != nil {
		return nil, firstErr
	}

	sortLoans(allLoans, sortByDueDate)
	return allLoans, nil
}

func fetchUsersByGroup(client *http.Client, cfg almaConfig) ([]almaUser, error) {
	allUsers := []almaUser{}
	offset := 0

	for {
		params := url.Values{}
		params.Set("apikey", cfg.APIKey)
		params.Set("q", "user_group~"+cfg.UserGroup)
		params.Set("limit", strconv.Itoa(pageSize))
		params.Set("offset", strconv.Itoa(offset))

		endpoint := cfg.BaseURL + "/almaws/v1/users?" + params.Encode()
		var resp almaUsersResponse
		if err := doXMLRequest(client, endpoint, &resp); err != nil {
			return nil, fmt.Errorf("failed to fetch users for group %s: %w", cfg.UserGroup, err)
		}

		allUsers = append(allUsers, resp.Users...)
		offset += len(resp.Users)
		if len(resp.Users) == 0 || offset >= resp.TotalRecordCount {
			break
		}
	}

	return allUsers, nil
}

func fetchUserLoans(client *http.Client, cfg almaConfig, userID string) ([]Loan, error) {
	allLoans := []Loan{}
	offset := 0

	for {
		params := url.Values{}
		params.Set("apikey", cfg.APIKey)
		params.Set("limit", strconv.Itoa(pageSize))
		params.Set("offset", strconv.Itoa(offset))
		params.Set("order_by", "due_date")

		endpoint := fmt.Sprintf("%s/almaws/v1/users/%s/loans?%s", cfg.BaseURL, url.PathEscape(userID), params.Encode())
		var resp almaItemLoansResponse
		if err := doXMLRequest(client, endpoint, &resp); err != nil {
			return nil, fmt.Errorf("failed to fetch loans for %s: %w", userID, err)
		}

		for _, item := range resp.Loans {
			dueTime, dueDisplay := parseDueDate(item.DueDate)
			allLoans = append(allLoans, Loan{
				Borrower:       item.UserID,
				Title:          strings.TrimSpace(item.Title),
				Barcode:        strings.TrimSpace(item.ItemBarcode),
				DueDate:        dueTime,
				DueDateDisplay: dueDisplay,
				Status:         deriveStatus(item.LoanStatus, item.ProcessStatus, dueTime),
				ProcessStatus:  strings.TrimSpace(item.ProcessStatus),
			})
		}

		offset += len(resp.Loans)
		if len(resp.Loans) == 0 || offset >= resp.TotalRecordCount {
			break
		}
	}

	return allLoans, nil
}

func doXMLRequest(client *http.Client, endpoint string, target any) error {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/xml")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("alma returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return xml.NewDecoder(resp.Body).Decode(target)
}

// parseDueDate normalizes Alma's date strings into something friendly for the table.
func parseDueDate(raw string) (time.Time, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, "-"
	}

	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, t.Format("2006-01-02")
		}
	}

	if len(raw) >= 10 {
		return time.Time{}, raw[:10]
	}
	return time.Time{}, raw
}

func deriveStatus(loanStatus, processStatus string, dueDate time.Time) string {
	status := strings.ToUpper(strings.TrimSpace(processStatus))
	if status == "" || status == "NORMAL" {
		status = strings.ToUpper(strings.TrimSpace(loanStatus))
	}
	if status == "ACTIVE" && !dueDate.IsZero() && dueDate.Before(time.Now()) {
		return "OVERDUE"
	}
	if status == "" {
		return "UNKNOWN"
	}
	return status
}

// filterLoans supports quick operational views like overdue, lost, and claims-returned.
func filterLoans(loans []Loan, mode filterMode) []Loan {
	filtered := make([]Loan, 0, len(loans))
	now := time.Now()

	for _, loan := range loans {
		statusText := strings.ToUpper(strings.Join([]string{loan.Status, loan.ProcessStatus, loan.Title}, " "))
		isOverdue := strings.Contains(statusText, "OVERDUE") || (!loan.DueDate.IsZero() && loan.DueDate.Before(now) && strings.Contains(strings.ToUpper(loan.Status), "ACTIVE"))

		include := false
		switch mode {
		case filterOverdue:
			include = isOverdue
		case filterLost:
			include = strings.Contains(statusText, "LOST")
		case filterClaims:
			include = strings.Contains(statusText, "CLAIM")
		default:
			include = true
		}

		if include {
			filtered = append(filtered, loan)
		}
	}

	return filtered
}

// searchLoans is intentionally broad so a demo audience can find things fast.
func searchLoans(loans []Loan, query string) []Loan {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return loans
	}

	filtered := make([]Loan, 0, len(loans))
	for _, loan := range loans {
		haystack := strings.ToLower(strings.Join([]string{
			loan.Borrower,
			loan.Title,
			loan.Barcode,
			loan.Status,
			loan.ProcessStatus,
			loan.DueDateDisplay,
		}, " "))
		if strings.Contains(haystack, query) {
			filtered = append(filtered, loan)
		}
	}
	return filtered
}

// sortLoans lets the presenter shift the story of the data in real time.
func sortLoans(loans []Loan, mode sortMode) {
	sort.SliceStable(loans, func(i, j int) bool {
		a, b := loans[i], loans[j]
		switch mode {
		case sortByUser:
			if a.Borrower == b.Borrower {
				return strings.ToLower(a.Title) < strings.ToLower(b.Title)
			}
			return strings.ToLower(a.Borrower) < strings.ToLower(b.Borrower)
		case sortByTitle:
			return strings.ToLower(a.Title) < strings.ToLower(b.Title)
		case sortByStatus:
			if a.Status == b.Status {
				return strings.ToLower(a.Title) < strings.ToLower(b.Title)
			}
			return strings.ToLower(a.Status) < strings.ToLower(b.Status)
		default:
			if a.DueDate.Equal(b.DueDate) {
				return strings.ToLower(a.Title) < strings.ToLower(b.Title)
			}
			if a.DueDate.IsZero() {
				return false
			}
			if b.DueDate.IsZero() {
				return true
			}
			return a.DueDate.Before(b.DueDate)
		}
	})
}

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error: %v", err)
		os.Exit(1)
	}
}