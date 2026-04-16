package main

import (
	"bufio"
	"bytes"
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
	"time"
    "unicode"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	refreshInterval       = 5 * time.Minute
	defaultAnalyticsLimit = 250
	maxAnalyticsLimit     = 1000
)

type itemRecord struct {
	Title             string
	Barcode           string
	CallNumber        string
	Status            string
	ProcessType       string
	Availability      string
	Library           string
	PermanentLocation string
	CurrentLocation   string
	DueDate           time.Time
	DueDateDisplay    string
	ProcessState      string
}

func (i itemRecord) TitleOrFallback() string {
	if strings.TrimSpace(i.Title) != "" {
		return i.Title
	}
	if strings.TrimSpace(i.CallNumber) != "" {
		return i.CallNumber
	}
	if strings.TrimSpace(i.Barcode) != "" {
		return i.Barcode
	}
	return "Untitled item"
}

type sortMode int

const (
	sortByProcessState sortMode = iota
	sortByTitle
	sortByCallNumber
)

func (s sortMode) Label() string {
	switch s {
	case sortByTitle:
		return "Title"
	case sortByCallNumber:
		return "Call Number"
	default:
		return "Process Type"
	}
}

const allFilterLabel = "All"

type almaConfig struct {
	BaseURL             string
	APIKey              string
	AnalyticsReportPath string
	AnalyticsFilter     string
	AnalyticsLimit      int
}

type model struct {
	table         table.Model
	items         []itemRecord
	width         int
	height        int
	loading       bool
	refreshing    bool
	spinner       spinner.Model
	searchInput   textinput.Model
	searchMode    bool
	searchQuery   string
	statusMessage string
	err           error
	sortMode      sortMode
	filterKey     string
	lastUpdated   time.Time
	visibleCount  int
	config        almaConfig
}

type analyticsXMLRow struct {
	Fields []analyticsXMLField `xml:",any"`
}

type analyticsXMLField struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type analyticsPage struct {
	ColumnHeadings   map[string]string
	Rows             []map[string]string
	ResumptionToken  string
	Finished         bool
	HadExplicitState bool
}

type dataLoadedMsg struct {
	items     []itemRecord
	err       error
	fetchedAt time.Time
}

type refreshMsg time.Time

func loadData(cfg almaConfig) tea.Cmd {
	return func() tea.Msg {
		items, err := fetchItemsFromAlma(cfg)
		return dataLoadedMsg{items: items, err: err, fetchedAt: time.Now()}
	}
}

func scheduleRefresh() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return refreshMsg(t)
	})
}

func initialModel() model {
	cfg := loadConfig()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	columns := []table.Column{
		{Title: "Process Type", Width: 16},
		{Title: "Title", Width: 40},
		{Title: "Barcode", Width: 16},
		{Title: "Call Number", Width: 20},
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
	ti.Placeholder = "Search title, barcode, call number, or status"
	ti.Prompt = "Search: "
	ti.Width = 56

	return model{
		table:       t,
		loading:     true,
		spinner:     s,
		searchInput: ti,
		sortMode:    sortByProcessState,
		filterKey:   allFilterLabel,
		config:      cfg,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, loadData(m.config), scheduleRefresh())
}

func (m *model) currentVisibleItems() []itemRecord {
	filtered := filterItems(m.items, m.filterKey)
	filtered = searchItems(filtered, m.searchQuery)
	sortItems(filtered, m.sortMode)
	return filtered
}

func (m *model) applyTable() {
	filtered := m.currentVisibleItems()
	m.visibleCount = len(filtered)

	rows := make([]table.Row, 0, len(filtered))
	for _, item := range filtered {
		rows = append(rows, table.Row{
			item.ProcessState,
			item.TitleOrFallback(),
			fallback(item.Barcode),
			fallback(item.CallNumber),
		})
	}

	if len(rows) == 0 {
		rows = []table.Row{{"-", "No matching items found", "-", "-"}}
	}

	m.table.SetRows(rows)
}

func (m *model) updateTableLayout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}

	usableWidth := maxInt(40, m.width-4-baseStyle.GetHorizontalFrameSize())
	tableHeight := maxInt(5, m.height-12-baseStyle.GetVerticalFrameSize())

	processWidth := clampInt(14, usableWidth/6, 24)
	barcodeWidth := clampInt(12, usableWidth/6, 20)
	callNumberWidth := clampInt(14, usableWidth/5, 26)
	titleWidth := maxInt(20, usableWidth-processWidth-barcodeWidth-callNumberWidth-3)

	m.table.SetWidth(usableWidth)
	m.table.SetHeight(tableHeight)
	m.table.SetColumns([]table.Column{
		{Title: "Process Type", Width: processWidth},
		{Title: "Title", Width: titleWidth},
		{Title: "Barcode", Width: barcodeWidth},
		{Title: "Call Number", Width: callNumberWidth},
	})
	m.searchInput.Width = maxInt(20, m.width-12)
}

func (m model) fetchInFlight() bool {
	return m.loading || m.refreshing
}

func (m model) currentFilterOrder() []string {
	return orderedProcessStateFilters(m.items)
}

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

		if filterKey, matched := directFilterKey(msg.String(), m.currentFilterOrder()); matched {
			m.filterKey = filterKey
			m.applyTable()
			return m, nil
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
			fileName := fmt.Sprintf("alma_item_status_%s.csv", time.Now().Format("20060102_150405"))
			if err := exportItemsToCSV(m.currentVisibleItems(), fileName); err != nil {
				m.statusMessage = "Export failed: " + err.Error()
			} else {
				m.statusMessage = "Exported CSV: " + fileName
			}
			return m, nil
		case "r":
			if m.fetchInFlight() {
				m.statusMessage = "Refresh skipped: Alma fetch already in progress"
				return m, nil
			}
			m.refreshing = true
			m.err = nil
			m.statusMessage = "Refreshing Alma data"
			return m, loadData(m.config)
		case "s":
			m.sortMode = (m.sortMode + 1) % 3
			m.applyTable()
			return m, nil
		case "f":
			m.filterKey = nextFilterKey(m.filterKey, m.currentFilterOrder())
			m.applyTable()
			return m, nil
		case "!":
			m.sortMode = sortByProcessState
			m.applyTable()
			return m, nil
		case "@":
			m.sortMode = sortByTitle
			m.applyTable()
			return m, nil
		case "#":
			m.sortMode = sortByCallNumber
			m.applyTable()
			return m, nil
		}

	case spinner.TickMsg:
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateTableLayout()
		return m, nil

	case refreshMsg:
		if m.fetchInFlight() {
			return m, scheduleRefresh()
		}
		m.refreshing = true
		m.err = nil
		m.statusMessage = "Refreshing Alma data"
		return m, tea.Batch(loadData(m.config), scheduleRefresh())

	case dataLoadedMsg:
		m.loading = false
		m.refreshing = false
		m.err = msg.err
		if msg.err == nil {
			m.items = msg.items
			m.lastUpdated = msg.fetchedAt
			m.applyTable()
		}
		return m, nil
	}

	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

var (
	baseStyle           = lipgloss.NewStyle().BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240"))
	headerStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	mutedStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	activeFilterStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Padding(0, 1)
	inactiveFilterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)
)

func (m model) View() string {
	if m.loading {
		return fmt.Sprintf("\n  %s Fetching Alma Analytics item status data...\n\n", m.spinner.View())
	}

	header := headerStyle.Render("Alma Item Status Pulse")
	summary := fmt.Sprintf("Showing %d of %d items • Sort: %s", m.visibleCount, len(m.items), m.sortMode.Label())
	if m.searchQuery != "" {
		summary += fmt.Sprintf(" • Search: %q", m.searchQuery)
	}
	if !m.lastUpdated.IsZero() {
		summary += fmt.Sprintf(" • Updated: %s", m.lastUpdated.Format("15:04:05"))
	}
	if m.refreshing {
		summary += fmt.Sprintf(" • %s Refreshing", m.spinner.View())
	}

	filterLine := renderFilterBar(processStateCounts(m.items), m.currentFilterOrder(), m.filterKey)

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

	footer := mutedStyle.Render("↑/↓ navigate • / search • 0-9 filter • f next filter • !/@/# sort • e export • esc close • ctrl+l clear • r refresh • q quit")

	return fmt.Sprintf("\n  %s\n  %s\n  %s%s%s\n  %s\n  %s\n%s\n\n  %s\n", header, summary, filterLine, errLine, statusLine, searchLine, baseStyle.Render(m.table.View()), footer)
}

func nextFilterKey(current string, ordered []string) string {
	for index, filterKey := range ordered {
		if filterKey == current {
			return ordered[(index+1)%len(ordered)]
		}
	}
	return allFilterLabel
}

func renderFilterBar(counts map[string]int, ordered []string, active string) string {
	parts := make([]string, 0, len(ordered))
	for index, filterKey := range ordered {
		label := fmt.Sprintf("%s %d", filterLabelWithShortcut(filterKey, index), counts[filterKey])
		if filterKey == active {
			parts = append(parts, activeFilterStyle.Render(label))
			continue
		}
		parts = append(parts, inactiveFilterStyle.Render(label))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}


func filterLabelWithShortcut(filterKey string, index int) string {
	shortcut := ""
	if index == 0 {
		shortcut = "0:"
	} else if index < 10 {
		shortcut = strconv.Itoa(index) + ":"
	}
	if shortcut == "" {
		return filterKey
	}
	return shortcut + " " + filterKey
}

func directFilterKey(input string, ordered []string) (string, bool) {
	if len(input) != 1 || !unicode.IsDigit(rune(input[0])) {
		return "", false
	}
	index := 0
	if input != "0" {
		parsed, err := strconv.Atoi(input)
		if err != nil || parsed >= len(ordered) {
			return "", false
		}
		index = parsed
	}
	if index >= len(ordered) {
		return "", false
	}
	return ordered[index], true
}

func loadConfig() almaConfig {
	for _, path := range []string{".env.sandbox", ".env"} {
		_ = loadEnvFile(path)
	}

	limit := defaultAnalyticsLimit
	if raw := strings.TrimSpace(os.Getenv("ALMA_ANALYTICS_LIMIT")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 25 && parsed <= maxAnalyticsLimit {
			limit = parsed
		}
	}

	return almaConfig{
		BaseURL:             strings.TrimRight(strings.TrimSpace(os.Getenv("ALMA_API_BASE_URL")), "/"),
		APIKey:              strings.TrimSpace(os.Getenv("ALMA_API_KEY")),
		AnalyticsReportPath: strings.TrimSpace(os.Getenv("ALMA_ANALYTICS_REPORT_PATH")),
		AnalyticsFilter:     strings.TrimSpace(os.Getenv("ALMA_ANALYTICS_FILTER")),
		AnalyticsLimit:      limit,
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(minimum, value, maximum int) int {
	return maxInt(minimum, minInt(value, maximum))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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

func exportItemsToCSV(items []itemRecord, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if err := writer.Write([]string{"Process Type", "Title", "Barcode", "Call Number", "Library", "Permanent Location", "Current Location", "Status", "Process Type (Raw)", "Availability", "Due Date"}); err != nil {
		return err
	}

	for _, item := range items {
		if err := writer.Write([]string{item.ProcessState, item.Title, item.Barcode, item.CallNumber, item.Library, item.PermanentLocation, item.CurrentLocation, item.Status, item.ProcessType, item.Availability, item.DueDateDisplay}); err != nil {
			return err
		}
	}

	return writer.Error()
}

func fetchItemsFromAlma(cfg almaConfig) ([]itemRecord, error) {
	if cfg.BaseURL == "" || cfg.APIKey == "" {
		return nil, fmt.Errorf("missing ALMA_API_BASE_URL or ALMA_API_KEY in .env or .env.sandbox")
	}
	if cfg.AnalyticsReportPath == "" {
		return nil, fmt.Errorf("missing ALMA_ANALYTICS_REPORT_PATH in .env or .env.sandbox")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	items, err := fetchAnalyticsItems(client, cfg)
	if err != nil {
		return nil, err
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("analytics report returned no usable item rows with a process type other than NONE; include title, barcode, call number, status, process type, and location fields in the report")
	}

	sortItems(items, sortByProcessState)
	return items, nil
}

func fetchAnalyticsItems(client *http.Client, cfg almaConfig) ([]itemRecord, error) {
	columnHeadings := map[string]string{}
	items := make([]itemRecord, 0, cfg.AnalyticsLimit)
	token := ""

	for {
		endpoint := buildAnalyticsEndpoint(cfg, token)
		body, err := doRawRequest(client, endpoint)
		if err != nil {
			return nil, err
		}

		page, err := parseAnalyticsPage(body)
		if err != nil {
			return nil, err
		}

		for key, value := range page.ColumnHeadings {
			columnHeadings[key] = value
		}

		for _, row := range page.Rows {
			item := buildItemRecord(row, columnHeadings)
			if !hasMeaningfulProcessType(item.ProcessType) {
				continue
			}
			if isAvailableItem(item) {
				continue
			}
			if strings.TrimSpace(item.Title) == "" && strings.TrimSpace(item.Barcode) == "" && strings.TrimSpace(item.CallNumber) == "" && strings.TrimSpace(item.ProcessType) == "" && strings.TrimSpace(item.Status) == "" {
				continue
			}
			items = append(items, item)
		}

		if page.ResumptionToken == "" || page.Finished || len(page.Rows) == 0 {
			break
		}
		token = decodeAnalyticsToken(page.ResumptionToken)
	}

	return items, nil
}

func buildAnalyticsEndpoint(cfg almaConfig, token string) string {
	params := url.Values{}
	params.Set("apikey", cfg.APIKey)
	params.Set("limit", strconv.Itoa(cfg.AnalyticsLimit))
	params.Set("col_names", "true")
	if strings.TrimSpace(token) != "" {
		params.Set("token", token)
	} else {
		params.Set("path", cfg.AnalyticsReportPath)
		if strings.TrimSpace(cfg.AnalyticsFilter) != "" {
			params.Set("filter", cfg.AnalyticsFilter)
		}
	}
	return cfg.BaseURL + "/almaws/v1/analytics/reports?" + params.Encode()
}

func hasMeaningfulProcessType(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	return value != "" && value != "NONE"
}

func isAvailableItem(item itemRecord) bool {
	combined := strings.ToUpper(strings.Join([]string{item.Status, item.ProcessType, item.Availability}, " "))
	if strings.Contains(combined, "UNAVAILABLE") || strings.Contains(combined, "NOT IN PLACE") || strings.EqualFold(strings.TrimSpace(item.Availability), "N") {
		return false
	}
	return strings.Contains(combined, "AVAILABLE") || strings.Contains(combined, "IN PLACE") || strings.EqualFold(strings.TrimSpace(item.Availability), "Y")
}

func decodeAnalyticsToken(token string) string {
	decoded, err := url.QueryUnescape(strings.TrimSpace(token))
	if err != nil {
		return strings.TrimSpace(token)
	}
	return decoded
}

func doRawRequest(client *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/xml")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("alma returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return body, nil
}

func parseAnalyticsPage(body []byte) (analyticsPage, error) {
	page := analyticsPage{ColumnHeadings: map[string]string{}}
	decoder := xml.NewDecoder(bytes.NewReader(body))

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return analyticsPage{}, err
		}

		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}

		switch start.Name.Local {
		case "ResumptionToken":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return analyticsPage{}, err
			}
			page.ResumptionToken = strings.TrimSpace(value)
		case "IsFinished":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return analyticsPage{}, err
			}
			page.HadExplicitState = true
			page.Finished = strings.EqualFold(strings.TrimSpace(value), "true")
		case "element":
			columnKey, heading := "", ""
			for _, attr := range start.Attr {
				switch strings.ToLower(attr.Name.Local) {
				case "name":
					columnKey = strings.TrimSpace(attr.Value)
				case "columnheading":
					heading = strings.TrimSpace(attr.Value)
				}
			}
			if columnKey != "" && heading != "" {
				page.ColumnHeadings[columnKey] = heading
			}
		case "Row":
			var row analyticsXMLRow
			if err := decoder.DecodeElement(&row, &start); err != nil {
				return analyticsPage{}, err
			}
			mapped := map[string]string{}
			for _, field := range row.Fields {
				mapped[field.XMLName.Local] = strings.TrimSpace(field.Value)
			}
			if len(mapped) > 0 {
				page.Rows = append(page.Rows, mapped)
			}
		}
	}

	if !page.HadExplicitState {
		page.Finished = true
	}
	return page, nil
}

func buildItemRecord(row map[string]string, columns map[string]string) itemRecord {
	item := itemRecord{
		Title:             valueForAliases(row, columns, "title"),
		Barcode:           valueForAliases(row, columns, "barcode", "item barcode"),
		CallNumber:        valueForAliases(row, columns, "call number"),
		Status:            valueForAliases(row, columns, "item status", "status"),
		ProcessType:       valueForAliases(row, columns, "process type", "process status"),
		Availability:      valueForAliases(row, columns, "availability", "availability status", "availability active"),
		Library:           valueForAliases(row, columns, "library", "library name"),
		PermanentLocation: valueForAliases(row, columns, "permanent location", "location permanent"),
		CurrentLocation:   valueForAliases(row, columns, "current location", "location active"),
	}

	dueRaw := valueForAliases(row, columns, "due date")
	item.DueDate, item.DueDateDisplay = parseDateValue(dueRaw)
	item.ProcessState = deriveProcessState(item)
	return item
}

func valueForAliases(row map[string]string, columns map[string]string, aliases ...string) string {
	bestScore := 0
	bestValue := ""
	for key, value := range row {
		if strings.TrimSpace(value) == "" {
			continue
		}

		heading := normalizeHeading(columns[key])
		fieldName := normalizeHeading(key)
		for _, alias := range aliases {
			normalizedAlias := normalizeHeading(alias)
			score := 0
			switch {
			case heading == normalizedAlias:
				score = 5
			case strings.Contains(heading, normalizedAlias):
				score = 4
			case fieldName == normalizedAlias:
				score = 3
			case strings.Contains(fieldName, normalizedAlias):
				score = 2
			}
			if score > bestScore {
				bestScore = score
				bestValue = strings.TrimSpace(value)
			}
		}
	}
	return bestValue
}

func normalizeHeading(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("\"", " ", ".", " ", "_", " ", "-", " ", "(", " ", ")", " ", "/", " ", ",", " ", ":", " ")
	value = replacer.Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

func parseDateValue(raw string) (time.Time, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, ""
	}

	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01-02 15:04:05", "01/02/2006", "1/2/2006"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, t.Format("2006-01-02")
		}
	}

	if len(raw) >= 10 {
		return time.Time{}, raw[:10]
	}
	return time.Time{}, raw
}

func deriveProcessState(item itemRecord) string {
	for _, candidate := range []string{item.ProcessType, item.Status, item.Availability} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && !strings.EqualFold(candidate, "none") {
			return candidate
		}
	}
	return "Unknown"
}

func processStateCounts(items []itemRecord) map[string]int {
	counts := map[string]int{allFilterLabel: len(items)}
	for _, item := range items {
		counts[item.ProcessState]++
	}
	return counts
}

func orderedProcessStateFilters(items []itemRecord) []string {
	counts := processStateCounts(items)
	filters := make([]string, 0, len(counts))
	for filterKey, count := range counts {
		if filterKey == allFilterLabel || count == 0 {
			continue
		}
		filters = append(filters, filterKey)
	}
	sort.Strings(filters)
	return append([]string{allFilterLabel}, filters...)
}

func filterItems(items []itemRecord, filterKey string) []itemRecord {
	if filterKey == "" || filterKey == allFilterLabel {
		return items
	}
	filtered := make([]itemRecord, 0, len(items))
	for _, item := range items {
		if item.ProcessState == filterKey {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func searchItems(items []itemRecord, query string) []itemRecord {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return items
	}

	filtered := make([]itemRecord, 0, len(items))
	for _, item := range items {
		haystack := strings.ToLower(strings.Join([]string{item.Title, item.Barcode, item.CallNumber, item.ProcessState, item.Status, item.ProcessType, item.Availability, item.Library, item.PermanentLocation, item.CurrentLocation, item.DueDateDisplay}, " "))
		if strings.Contains(haystack, query) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func sortItems(items []itemRecord, mode sortMode) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch mode {
		case sortByTitle:
			return strings.ToLower(a.TitleOrFallback()) < strings.ToLower(b.TitleOrFallback())
		case sortByCallNumber:
			if a.CallNumber == b.CallNumber {
				return strings.ToLower(a.TitleOrFallback()) < strings.ToLower(b.TitleOrFallback())
			}
			return strings.ToLower(a.CallNumber) < strings.ToLower(b.CallNumber)
		default:
			if a.ProcessState == b.ProcessState {
				return strings.ToLower(a.TitleOrFallback()) < strings.ToLower(b.TitleOrFallback())
			}
			return strings.ToLower(a.ProcessState) < strings.ToLower(b.ProcessState)
		}
	})
}

func fallback(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error: %v", err)
		os.Exit(1)
	}
}
