package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

func getDBURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://admin:secret@localhost:5433/onodi_library"
}

type Book struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Author         string  `json:"author"`
	ReadPages      int     `json:"read_pages"`
	TotalPages     int     `json:"total_pages"`
	Status         string  `json:"status"`
	CoverURL       *string `json:"cover_url"`
	Rating         *int    `json:"rating"`
	ReviewText     *string `json:"review_text"`
	Genre          *string `json:"genre"`
	SeriesName     *string `json:"series_name"`
	SeriesPosition *int    `json:"series_position"`
	DateRead       *string `json:"date_read"`
}

type ProgressUpdate struct {
	ReadPages  int     `json:"read_pages"`
	Status     string  `json:"status"`
	Rating     *int    `json:"rating"`
	ReviewText *string `json:"review_text"`
}

type LogEntry struct {
	ID         string `json:"id"`
	BookID     string `json:"book_id"`
	Content    string `json:"content"`
	PageNumber *int   `json:"page_number"`
	CreatedAt  string `json:"created_at"`
}

type LogEntryWithBook struct {
	ID         string  `json:"id"`
	BookID     string  `json:"book_id"`
	BookTitle  string  `json:"book_title"`
	CoverURL   *string `json:"cover_url"`
	Content    string  `json:"content"`
	PageNumber *int    `json:"page_number"`
	TotalPages int     `json:"total_pages"`
	CreatedAt  string  `json:"created_at"`
}

type OLDoc struct {
	Title   string   `json:"title"`
	Authors []string `json:"author_name"`
	Pages   *int     `json:"number_of_pages_median"`
	CoverID *int     `json:"cover_i"`
	Subject []string `json:"subject"`
}

type OLResponse struct {
	Docs []OLDoc `json:"docs"`
}

type BookSuggestion struct {
	Title      string  `json:"title"`
	Author     string  `json:"author"`
	TotalPages int     `json:"total_pages"`
	CoverURL   *string `json:"cover_url"`
	Genre      *string `json:"genre"`
}

// Google Books structures
type GBVolume struct {
	VolumeInfo struct {
		Title      string   `json:"title"`
		Authors    []string `json:"authors"`
		Categories []string `json:"categories"`
		PageCount  int      `json:"pageCount"`
		ImageLinks struct {
			Thumbnail string `json:"thumbnail"`
		} `json:"imageLinks"`
		SeriesInfo *struct {
			BookDisplayNumber string `json:"bookDisplayNumber"`
			VolumeSeries      []struct {
				SeriesID       string  `json:"seriesId"`
				SeriesBookType string  `json:"seriesBookType"`
				OrderNumber    float64 `json:"orderNumber"`
			} `json:"volumeSeries"`
		} `json:"seriesInfo"`
	} `json:"volumeInfo"`
}

type GBResponse struct {
	Items []GBVolume `json:"items"`
}

type SeriesBook struct {
	Title      string  `json:"title"`
	Author     string  `json:"author"`
	Position   int     `json:"position"`
	CoverURL   *string `json:"cover_url"`
	TotalPages int     `json:"total_pages"`
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

// Extraire un genre lisible depuis les subjects Open Library
func extractGenre(subjects []string) *string {
	priority := []string{
		"Fiction", "Fantasy", "Science Fiction", "Mystery", "Romance",
		"Thriller", "Horror", "Historical", "Biography", "Self-help",
		"Nonfiction", "Literary fiction", "Adventure", "Graphic novel",
		"Young adult", "Children", "Poetry", "Philosophy", "History",
	}
	for _, p := range priority {
		for _, s := range subjects {
			if strings.Contains(strings.ToLower(s), strings.ToLower(p)) {
				result := p
				return &result
			}
		}
	}
	if len(subjects) > 0 {
		// Prendre le premier sujet court
		for _, s := range subjects {
			if len(s) < 30 {
				result := s
				return &result
			}
		}
	}
	return nil
}

// Rechercher la série d'un livre via Google Books
func fetchSeriesFromGoogleBooks(title, author string) ([]SeriesBook, error) {
	q := url.QueryEscape(fmt.Sprintf("%s %s", title, author))
	apiURL := fmt.Sprintf("https://www.googleapis.com/books/v1/volumes?q=%s&maxResults=1", q)

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var gbResp GBResponse
	if err := json.Unmarshal(body, &gbResp); err != nil || len(gbResp.Items) == 0 {
		return nil, fmt.Errorf("no results")
	}

	vol := gbResp.Items[0]
	if vol.VolumeInfo.SeriesInfo == nil || len(vol.VolumeInfo.SeriesInfo.VolumeSeries) == 0 {
		return nil, fmt.Errorf("no series info")
	}

	seriesID := vol.VolumeInfo.SeriesInfo.VolumeSeries[0].SeriesID
	if seriesID == "" {
		return nil, fmt.Errorf("no series id")
	}

	// Récupérer les livres de la série
	seriesURL := fmt.Sprintf("https://www.googleapis.com/books/v1/volumes?q=seriesid:%s&maxResults=10&orderBy=newest", seriesID)
	resp2, err := http.Get(seriesURL)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)

	var seriesResp GBResponse
	if err := json.Unmarshal(body2, &seriesResp); err != nil {
		return nil, err
	}

	var books []SeriesBook
	for i, item := range seriesResp.Items {
		vi := item.VolumeInfo
		if len(vi.Authors) == 0 {
			continue
		}
		var coverURL *string
		if vi.ImageLinks.Thumbnail != "" {
			c := strings.Replace(vi.ImageLinks.Thumbnail, "http://", "https://", 1)
			coverURL = &c
		}
		pos := i + 1
		if item.VolumeInfo.SeriesInfo != nil && len(item.VolumeInfo.SeriesInfo.VolumeSeries) > 0 {
			pos = int(item.VolumeInfo.SeriesInfo.VolumeSeries[0].OrderNumber)
		}
		books = append(books, SeriesBook{
			Title:      vi.Title,
			Author:     vi.Authors[0],
			Position:   pos,
			CoverURL:   coverURL,
			TotalPages: vi.PageCount,
		})
	}
	return books, nil
}

// Fallback : chercher la série via Open Library
func fetchSeriesFromOpenLibrary(title, author string) ([]SeriesBook, error) {
	q := url.QueryEscape(fmt.Sprintf("%s %s", title, author))
	apiURL := fmt.Sprintf("https://openlibrary.org/search.json?q=%s&limit=10&fields=title,author_name,number_of_pages_median,cover_i,series", q)

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	type OLSeriesDoc struct {
		Title   string   `json:"title"`
		Authors []string `json:"author_name"`
		Pages   *int     `json:"number_of_pages_median"`
		CoverID *int     `json:"cover_i"`
		Series  []string `json:"series"`
	}
	type OLSeriesResp struct {
		Docs []OLSeriesDoc `json:"docs"`
	}

	var olResp OLSeriesResp
	if err := json.Unmarshal(body, &olResp); err != nil || len(olResp.Docs) == 0 {
		return nil, fmt.Errorf("no results")
	}

	// Trouver le livre et sa série
	var seriesName string
	for _, doc := range olResp.Docs {
		if len(doc.Series) > 0 {
			seriesName = doc.Series[0]
			break
		}
	}
	if seriesName == "" {
		return nil, fmt.Errorf("no series found")
	}

	// Chercher tous les livres de cette série
	q2 := url.QueryEscape(seriesName)
	seriesURL := fmt.Sprintf("https://openlibrary.org/search.json?q=%s&limit=10&fields=title,author_name,number_of_pages_median,cover_i,series", q2)
	resp2, err := http.Get(seriesURL)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)

	var seriesResp OLSeriesResp
	if err := json.Unmarshal(body2, &seriesResp); err != nil {
		return nil, err
	}

	var books []SeriesBook
	for i, doc := range seriesResp.Docs {
		if len(doc.Series) == 0 || !strings.EqualFold(doc.Series[0], seriesName) {
			continue
		}
		if len(doc.Authors) == 0 {
			continue
		}
		pages := 0
		if doc.Pages != nil {
			pages = *doc.Pages
		}
		var coverURL *string
		if doc.CoverID != nil {
			s := fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", *doc.CoverID)
			coverURL = &s
		}
		books = append(books, SeriesBook{
			Title:      doc.Title,
			Author:     doc.Authors[0],
			Position:   i + 1,
			CoverURL:   coverURL,
			TotalPages: pages,
		})
	}
	return books, nil
}

func main() {
	conn, err := pgxpool.New(context.Background(), getDBURL())
	if err != nil {
		log.Fatalf("Échec connexion PostgreSQL: %v\n", err)
	}
	defer conn.Close()
	log.Println("Connexion Onodi établie.")

	router := gin.Default()
	router.Use(corsMiddleware())

	// GET /search
	router.GET("/search", func(c *gin.Context) {
		query := c.Query("q")
		if query == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Paramètre q requis"})
			return
		}
		apiURL := fmt.Sprintf(
			"https://openlibrary.org/search.json?q=%s&limit=8&fields=title,author_name,number_of_pages_median,cover_i,subject",
			url.QueryEscape(query),
		)
		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Get(apiURL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur Open Library"})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var olResp OLResponse
		if err := json.Unmarshal(body, &olResp); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur parsing"})
			return
		}
		suggestions := []BookSuggestion{}
		for _, doc := range olResp.Docs {
			if doc.Title == "" || len(doc.Authors) == 0 {
				continue
			}
			pages := 0
			if doc.Pages != nil {
				pages = *doc.Pages
			}
			var coverURL *string
			if doc.CoverID != nil {
				s := fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", *doc.CoverID)
				coverURL = &s
			}
			suggestions = append(suggestions, BookSuggestion{
				Title: doc.Title, Author: doc.Authors[0],
				TotalPages: pages, CoverURL: coverURL,
				Genre: extractGenre(doc.Subject),
			})
		}
		c.JSON(http.StatusOK, suggestions)
	})

	// GET /books/:id/series
	router.GET("/books/:id/series", func(c *gin.Context) {
		id := c.Param("id")

		var title, author string
		err := conn.QueryRow(context.Background(),
			"SELECT title, author FROM books WHERE id=$1", id).Scan(&title, &author)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Livre introuvable"})
			return
		}

		// Essai Google Books d'abord
		books, err := fetchSeriesFromGoogleBooks(title, author)
		if err != nil || len(books) == 0 {
			// Fallback Open Library
			books, err = fetchSeriesFromOpenLibrary(title, author)
			if err != nil || len(books) == 0 {
				c.JSON(http.StatusOK, []SeriesBook{})
				return
			}
		}
		c.JSON(http.StatusOK, books)
	})

	// GET /books
	router.GET("/books", func(c *gin.Context) {
		rows, err := conn.Query(context.Background(),
			`SELECT id, title, author, read_pages, total_pages, status, cover_url,
			        rating, review_text, genre, series_name, series_position,
			        CAST(date_read AS TEXT) as date_read
			 FROM books ORDER BY COALESCE(date_read, created_at::date) DESC, created_at DESC`)
		if err != nil {
			log.Printf("GET /books query error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var books []Book
		for rows.Next() {
			var b Book
			if err := rows.Scan(&b.ID, &b.Title, &b.Author, &b.ReadPages, &b.TotalPages,
				&b.Status, &b.CoverURL, &b.Rating, &b.ReviewText,
				&b.Genre, &b.SeriesName, &b.SeriesPosition, &b.DateRead); err != nil {
				log.Printf("GET /books scan error: %v", err)
				continue
			}
			books = append(books, b)
		}
		if books == nil {
			books = []Book{}
		}
		c.JSON(http.StatusOK, books)
	})

	// POST /books
	router.POST("/books", func(c *gin.Context) {
		var b Book
		if err := c.BindJSON(&b); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "JSON invalide"})
			return
		}
		if b.Status == "" {
			b.Status = "to_read"
		}
		err := conn.QueryRow(context.Background(),
			`INSERT INTO books (title, author, total_pages, status, cover_url, genre)
			 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			b.Title, b.Author, b.TotalPages, b.Status, b.CoverURL, b.Genre).Scan(&b.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Échec insertion"})
			return
		}
		c.JSON(http.StatusCreated, b)
	})

	// PUT /books/:id/progress
	router.PUT("/books/:id/progress", func(c *gin.Context) {
		id := c.Param("id")
		var u ProgressUpdate
		if err := c.BindJSON(&u); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "JSON invalide"})
			return
		}
		tag, err := conn.Exec(context.Background(),
			`UPDATE books SET read_pages=$1, status=$2, rating=$3, review_text=$4,
			 updated_at=CURRENT_TIMESTAMP WHERE id=$5`,
			u.ReadPages, u.Status, u.Rating, u.ReviewText, id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Échec mise à jour"})
			return
		}
		if tag.RowsAffected() == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "Livre introuvable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "OK"})
	})

	// DELETE /books/:id
	router.DELETE("/books/:id", func(c *gin.Context) {
		id := c.Param("id")
		tag, err := conn.Exec(context.Background(), "DELETE FROM books WHERE id=$1", id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Échec suppression"})
			return
		}
		if tag.RowsAffected() == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "Livre introuvable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "OK"})
	})

	// GET /books/:id/log
	router.GET("/books/:id/log", func(c *gin.Context) {
		id := c.Param("id")
		rows, err := conn.Query(context.Background(),
			`SELECT id, book_id, content, page_number, created_at FROM reading_log
			 WHERE book_id=$1 ORDER BY created_at DESC`, id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Échec extraction"})
			return
		}
		defer rows.Close()
		var entries []LogEntry
		for rows.Next() {
			var e LogEntry
			var createdAt interface{}
			if err := rows.Scan(&e.ID, &e.BookID, &e.Content, &e.PageNumber, &createdAt); err != nil {
				continue
			}
			e.CreatedAt = fmt.Sprintf("%v", createdAt)
			entries = append(entries, e)
		}
		if entries == nil {
			entries = []LogEntry{}
		}
		c.JSON(http.StatusOK, entries)
	})

	// POST /books/:id/log
	router.POST("/books/:id/log", func(c *gin.Context) {
		id := c.Param("id")
		var entry LogEntry
		if err := c.BindJSON(&entry); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "JSON invalide"})
			return
		}
		var createdAt interface{}
		err := conn.QueryRow(context.Background(),
			`INSERT INTO reading_log (book_id, content, page_number) VALUES ($1, $2, $3) RETURNING id, created_at`,
			id, entry.Content, entry.PageNumber).Scan(&entry.ID, &createdAt)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Échec insertion"})
			return
		}
		entry.CreatedAt = fmt.Sprintf("%v", createdAt)
		if entry.PageNumber != nil {
			conn.Exec(context.Background(),
				`UPDATE books SET read_pages=$1, updated_at=CURRENT_TIMESTAMP WHERE id=$2`,
				*entry.PageNumber, id)
		}
		entry.BookID = id
		c.JSON(http.StatusCreated, entry)
	})

	// GET /log
	router.GET("/log", func(c *gin.Context) {
		rows, err := conn.Query(context.Background(),
			`SELECT rl.id, rl.book_id, b.title, b.cover_url, rl.content, rl.page_number, b.total_pages, rl.created_at
			 FROM reading_log rl
			 JOIN books b ON b.id = rl.book_id
			 ORDER BY rl.created_at DESC LIMIT 50`)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Échec extraction"})
			return
		}
		defer rows.Close()
		var entries []LogEntryWithBook
		for rows.Next() {
			var e LogEntryWithBook
			var createdAt interface{}
			if err := rows.Scan(&e.ID, &e.BookID, &e.BookTitle, &e.CoverURL,
				&e.Content, &e.PageNumber, &e.TotalPages, &createdAt); err != nil {
				continue
			}
			e.CreatedAt = fmt.Sprintf("%v", createdAt)
			entries = append(entries, e)
		}
		if entries == nil {
			entries = []LogEntryWithBook{}
		}
		c.JSON(http.StatusOK, entries)
	})

	// GET /stats
	router.GET("/stats", func(c *gin.Context) {
		type MonthStat struct {
			Month string `json:"month"`
			Count int    `json:"count"`
			Pages int    `json:"pages"`
		}
		type GenreStat struct {
			Genre string `json:"genre"`
			Count int    `json:"count"`
		}
		type AuthorStat struct {
			Author string `json:"author"`
			Count  int    `json:"count"`
		}
		type BookMini struct {
			Title    string  `json:"title"`
			CoverURL *string `json:"cover_url"`
		}
		type DayStat struct {
			Date  string     `json:"date"`
			Count int        `json:"count"`
			Books []BookMini `json:"books"`
		}

		// Livres lus par mois (12 derniers mois) avec couvertures
		type MonthStatWithCovers struct {
			Month     string   `json:"month"`
			Count     int      `json:"count"`
			Pages     int      `json:"pages"`
			CoverURLs []string `json:"cover_urls"`
		}
		monthRows, _ := conn.Query(context.Background(),
			`WITH months AS (
			  SELECT TO_CHAR(DATE_TRUNC('month', NOW()) - (n || ' months')::interval, 'YYYY-MM') as month
			  FROM generate_series(0, 11) n
			)
			SELECT m.month,
			       COALESCE(COUNT(b.id), 0) as count,
			       COALESCE(SUM(b.total_pages), 0) as pages,
			       ARRAY_REMOVE(ARRAY_AGG(b.cover_url ORDER BY b.updated_at DESC), NULL) as covers
			FROM months m
			LEFT JOIN books b ON TO_CHAR(b.updated_at, 'YYYY-MM') = m.month AND b.status='read'
			GROUP BY m.month ORDER BY m.month ASC`)
		defer monthRows.Close()
		var monthStats []MonthStatWithCovers
		for monthRows.Next() {
			var s MonthStatWithCovers
			var covers []string
			monthRows.Scan(&s.Month, &s.Count, &s.Pages, &covers)
			if covers == nil {
				covers = []string{}
			}
			s.CoverURLs = covers
			monthStats = append(monthStats, s)
		}

		// Genres
		genreRows, _ := conn.Query(context.Background(),
			`SELECT genre, COUNT(*) as count FROM books
			 WHERE genre IS NOT NULL GROUP BY genre ORDER BY count DESC`)
		defer genreRows.Close()
		var genreStats []GenreStat
		for genreRows.Next() {
			var s GenreStat
			genreRows.Scan(&s.Genre, &s.Count)
			genreStats = append(genreStats, s)
		}

		// Auteurs
		authorRows, _ := conn.Query(context.Background(),
			`SELECT author, COUNT(*) as count FROM books
			 WHERE status='read' GROUP BY author ORDER BY count DESC LIMIT 10`)
		defer authorRows.Close()
		var authorStats []AuthorStat
		for authorRows.Next() {
			var s AuthorStat
			authorRows.Scan(&s.Author, &s.Count)
			authorStats = append(authorStats, s)
		}

		// Activité par jour avec livres associés
		dayRows, _ := conn.Query(context.Background(),
			`SELECT DATE(rl.created_at) as day, COUNT(DISTINCT rl.id) as count,
			        ARRAY_AGG(DISTINCT b.title) as titles,
			        ARRAY_REMOVE(ARRAY_AGG(DISTINCT b.cover_url), NULL) as covers
			 FROM reading_log rl
			 JOIN books b ON b.id = rl.book_id
			 WHERE rl.created_at >= NOW() - INTERVAL '365 days'
			 GROUP BY day ORDER BY day`)
		defer dayRows.Close()
		var dayStats []DayStat
		for dayRows.Next() {
			var s DayStat
			var day interface{}
			var titles []string
			var covers []string
			dayRows.Scan(&day, &s.Count, &titles, &covers)
			s.Date = fmt.Sprintf("%v", day)[:10]
			if covers == nil {
				covers = []string{}
			}
			if titles == nil {
				titles = []string{}
			}
			for i, title := range titles {
				var coverURL *string
				if i < len(covers) && covers[i] != "" {
					c := covers[i]
					coverURL = &c
				}
				s.Books = append(s.Books, BookMini{Title: title, CoverURL: coverURL})
			}
			dayStats = append(dayStats, s)
		}

		// Stats globales
		var totalRead, totalPages, thisMonthBooks, thisMonthPages, thisYearBooks int
		conn.QueryRow(context.Background(),
			`SELECT COUNT(*), COALESCE(SUM(total_pages),0) FROM books WHERE status='read'`).
			Scan(&totalRead, &totalPages)
		conn.QueryRow(context.Background(),
			`SELECT COUNT(*), COALESCE(SUM(total_pages),0) FROM books
			 WHERE status='read' AND DATE_TRUNC('month', updated_at) = DATE_TRUNC('month', NOW())`).
			Scan(&thisMonthBooks, &thisMonthPages)
		conn.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM books
			 WHERE status='read' AND DATE_TRUNC('year', updated_at) = DATE_TRUNC('year', NOW())`).
			Scan(&thisYearBooks)

		c.JSON(http.StatusOK, gin.H{
			"total_read":       totalRead,
			"total_pages":      totalPages,
			"this_month_books": thisMonthBooks,
			"this_month_pages": thisMonthPages,
			"this_year_books":  thisYearBooks,
			"books_by_month":   monthStats,
			"books_by_genre":   genreStats,
			"books_by_author":  authorStats,
			"activity_by_day":  dayStats,
		})
	})

	// POST /import/goodreads — Import CSV Goodreads
	router.POST("/import/goodreads", func(c *gin.Context) {
		file, err := c.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Fichier requis"})
			return
		}

		f, err := file.Open()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur ouverture fichier"})
			return
		}
		defer f.Close()

		reader := csv.NewReader(f)
		records, err := reader.ReadAll()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "CSV invalide"})
			return
		}

		if len(records) < 2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "CSV vide"})
			return
		}

		// Mapper les colonnes
		headers := records[0]
		colIdx := map[string]int{}
		for i, h := range headers {
			colIdx[h] = i
		}

		get := func(row []string, col string) string {
			i, ok := colIdx[col]
			if !ok || i >= len(row) {
				return ""
			}
			return strings.TrimSpace(row[i])
		}

		// Mapping statuts Goodreads → Onodi
		shelfToStatus := map[string]string{
			"to-read":           "to_read",
			"currently-reading": "reading",
			"read":              "read",
			"gave-up":           "dnf",
		}

		imported := 0
		skipped := 0

		for _, row := range records[1:] {
			title := get(row, "Title")
			author := get(row, "Author")
			if title == "" || author == "" {
				skipped++
				continue
			}

			// Nettoyage titre (ex: "La 5e vague (La 5e vague, #1)" → garder tel quel)
			pagesStr := get(row, "Number of Pages")
			pages := 0
			fmt.Sscanf(pagesStr, "%d", &pages)

			ratingStr := get(row, "My Rating")
			rating := 0
			fmt.Sscanf(ratingStr, "%d", &rating)

			shelf := get(row, "Exclusive Shelf")
			status, ok := shelfToStatus[shelf]
			if !ok {
				status = "to_read"
			}

			review := get(row, "My Review")
			// Nettoyer HTML basique
			review = strings.ReplaceAll(review, "<br/>", " ")
			review = strings.ReplaceAll(review, "<br>", " ")

			// Vérifier si le livre existe déjà (par titre + auteur)
			var exists bool
			conn.QueryRow(context.Background(),
				"SELECT EXISTS(SELECT 1 FROM books WHERE LOWER(title)=LOWER($1) AND LOWER(author)=LOWER($2))",
				title, author).Scan(&exists)
			if exists {
				skipped++
				continue
			}

			// Parser la date de fin
			dateReadStr := get(row, "Date Read")
			var dateReadPtr *string
			if dateReadStr != "" {
				// Format Goodreads: 2026/06/19 → 2026-06-19
				normalized := strings.ReplaceAll(dateReadStr, "/", "-")
				dateReadPtr = &normalized
			}

			// Insérer
			var ratingPtr *int
			if rating > 0 {
				ratingPtr = &rating
			}
			var reviewPtr *string
			if review != "" {
				reviewPtr = &review
			}

			readPages := 0
			if status == "read" {
				readPages = pages
			}

			_, err := conn.Exec(context.Background(),
				`INSERT INTO books (title, author, total_pages, read_pages, status, rating, review_text, date_read)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				title, author, pages, readPages, status, ratingPtr, reviewPtr, dateReadPtr)
			if err != nil {
				skipped++
				continue
			}
			imported++
		}

		c.JSON(http.StatusOK, gin.H{
			"imported": imported,
			"skipped":  skipped,
			"message":  fmt.Sprintf("%d livres importés, %d ignorés (doublons ou erreurs)", imported, skipped),
		})

		// Lancer l'enrichissement automatiquement après l'import
		if imported > 0 {
			go func() {
				log.Printf("Enrichissement automatique de %d livres...", imported)
				enrichRows, err := conn.Query(context.Background(),
					`SELECT id, title, author FROM books WHERE cover_url IS NULL OR genre IS NULL LIMIT 300`)
				if err != nil {
					return
				}
				type BookToEnrich struct{ ID, Title, Author string }
				var booksToEnrich []BookToEnrich
				for enrichRows.Next() {
					var b BookToEnrich
					enrichRows.Scan(&b.ID, &b.Title, &b.Author)
					booksToEnrich = append(booksToEnrich, b)
				}
				enrichRows.Close()
				client := &http.Client{Timeout: 10 * time.Second}
				enriched := 0
				for _, book := range booksToEnrich {
					q := url.QueryEscape(fmt.Sprintf("%s %s", book.Title, book.Author))
					apiURL := fmt.Sprintf(
						"https://openlibrary.org/search.json?q=%s&limit=1&fields=title,author_name,cover_i,subject",
						q,
					)
					resp, err := client.Get(apiURL)
					if err != nil {
						time.Sleep(500 * time.Millisecond)
						continue
					}
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					var olResp OLResponse
					if err := json.Unmarshal(body, &olResp); err != nil || len(olResp.Docs) == 0 {
						time.Sleep(300 * time.Millisecond)
						continue
					}
					doc := olResp.Docs[0]
					var coverURL *string
					if doc.CoverID != nil {
						s := fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", *doc.CoverID)
						coverURL = &s
					}
					genre := extractGenre(doc.Subject)
					conn.Exec(context.Background(),
						`UPDATE books SET cover_url = COALESCE(cover_url, $1), genre = COALESCE(genre, $2) WHERE id = $3`,
						coverURL, genre, book.ID)
					enriched++
					time.Sleep(1000 * time.Millisecond)
				}
				log.Printf("Enrichissement terminé: %d livres mis à jour", enriched)
			}()
		}
	})

	// POST /enrich — Enrichit les livres sans couverture/genre via Open Library (arrière-plan)
	router.POST("/enrich", func(c *gin.Context) {
		// Récupérer les livres sans couverture ou sans genre
		rows, err := conn.Query(context.Background(),
			`SELECT id, title, author FROM books 
			 WHERE cover_url IS NULL OR genre IS NULL 
			 LIMIT 300`)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Erreur lecture"})
			return
		}
		type BookToEnrich struct {
			ID     string
			Title  string
			Author string
		}
		var books []BookToEnrich
		for rows.Next() {
			var b BookToEnrich
			rows.Scan(&b.ID, &b.Title, &b.Author)
			books = append(books, b)
		}
		rows.Close()

		total := len(books)
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Enrichissement de %d livres lancé en arrière-plan", total)})

		// Lancer en goroutine pour ne pas bloquer la réponse
		go func() {
			enriched := 0
			client := &http.Client{Timeout: 10 * time.Second}
			for _, book := range books {
				q := url.QueryEscape(fmt.Sprintf("%s %s", book.Title, book.Author))
				apiURL := fmt.Sprintf(
					"https://openlibrary.org/search.json?q=%s&limit=1&fields=title,author_name,cover_i,subject",
					q,
				)
				resp, err := client.Get(apiURL)
				if err != nil {
					time.Sleep(500 * time.Millisecond)
					continue
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				var olResp OLResponse
				if err := json.Unmarshal(body, &olResp); err != nil || len(olResp.Docs) == 0 {
					time.Sleep(300 * time.Millisecond)
					continue
				}

				doc := olResp.Docs[0]
				var coverURL *string
				if doc.CoverID != nil {
					s := fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", *doc.CoverID)
					coverURL = &s
				}
				genre := extractGenre(doc.Subject)

				conn.Exec(context.Background(),
					`UPDATE books SET 
					 cover_url = COALESCE(cover_url, $1),
					 genre = COALESCE(genre, $2)
					 WHERE id = $3`,
					coverURL, genre, book.ID)

				enriched++
				// Pause pour ne pas surcharger Open Library
				time.Sleep(1000 * time.Millisecond)
			}
			log.Printf("Enrichissement terminé: %d/%d livres mis à jour", enriched, total)
		}()
	})

	// GET /enrich/status — Combien de livres restent à enrichir
	router.GET("/enrich/status", func(c *gin.Context) {
		var withoutCover, withoutGenre, total int
		conn.QueryRow(context.Background(), "SELECT COUNT(*) FROM books").Scan(&total)
		conn.QueryRow(context.Background(), "SELECT COUNT(*) FROM books WHERE cover_url IS NULL").Scan(&withoutCover)
		conn.QueryRow(context.Background(), "SELECT COUNT(*) FROM books WHERE genre IS NULL").Scan(&withoutGenre)
		c.JSON(http.StatusOK, gin.H{
			"total":           total,
			"without_cover":   withoutCover,
			"without_genre":   withoutGenre,
			"enriched_covers": total - withoutCover,
			"enriched_genres": total - withoutGenre,
		})
	})

	router.Run(":8080")
}
