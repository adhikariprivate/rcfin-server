package postgres

import (
	"context"
	"database/sql"
	"github.com/jmoiron/sqlx"
	"log"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/idhash"
)

func (i *PostgresRepo) dbInsertItem(tx *sqlx.Tx, item *model.Item) error {
	// item.Genrestring = strings.Join(item.Genre, ",")
	_, err := tx.NamedExec(
		`INSERT INTO items(id, name, votes, genre, rating, year, nfotime, `+
			`		firstvideo, lastvideo)`+
			`VALUES (:id, :name, :votes, :genre, :rating, :year, :nfotime, `+
			`		:firstvideo, :lastvideo)`, item)
	return err
}

func (i *PostgresRepo) dbUpdateItem(tx *sqlx.Tx, item *model.Item) error {
	// item.Genrestring = strings.Join(item.Genre, ",")
	_, err := tx.NamedExec(
		`UPDATE items SET votes = :votes, genre = :genre, rating = :rating, `+
			`		year = :year, nfotime = :nfotime, `+
			`		firstvideo = :firstvideo, lastvideo = :lastvideo `+
			`		WHERE name = :name`, item)
	return err
}

func (i *PostgresRepo) DbLoadItem(item *model.Item) {
	var existingID string
	err := i.dbReadHandle.Get(&existingID,
		rebind("SELECT id FROM items WHERE name=? LIMIT 1"), item.Name)
	if err == nil {
		return
	}
	if err != sql.ErrNoRows {
		log.Printf("dbLoadItem (%s): %v", item.Name, err)
		return
	}

	item.ID = idhash.IdHash(item.Name)
	tx, err := i.dbWriteHandle.BeginTxx(context.Background(), nil)
	if err != nil {
		log.Printf("dbLoadItem (%s): begin transaction: %v", item.Name, err)
		return
	}
	defer tx.Rollback()
	if err := i.dbInsertItem(tx, item); err != nil {
		log.Printf("dbLoadItem: insert name=%s id=%s: %v", item.Name, item.ID, err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("dbLoadItem (%s): commit: %v", item.Name, err)
	}
}

// Check NFO file.
// func (d *Database) itemCheckNfo(item *collection.Item) (updated bool) {
// 	if item.NfoPath == "" {
// 		return
// 	}
// 	if item.NfoTime > 0 {
// 		fi, err := os.Stat(item.NfoPath)
// 		if err == nil && TimeToUnixMS(fi.ModTime()) == item.NfoTime {
// 			return
// 		}
// 	}

// 	fh, err := os.Open(item.NfoPath)
// 	if err != nil {
// 		fmt.Printf("XXX DEBUG open %s: %s\n", item.NfoPath, err)
// 		return
// 	}
// 	nfo := nfo.DecodeNfo(fh)
// 	ftime := int64(0)
// 	fi, err := fh.Stat()
// 	if err == nil {
// 		ftime = TimeToUnixMS(fi.ModTime())
// 	}
// 	fh.Close()
// 	if nfo == nil {
// 		fmt.Printf("XXX DEBUG XML failed %s\n", item.Name)
// 		return
// 	}
// 	otime := item.NfoTime

// 	item.NfoTime = ftime
// 	item.Genre = nfo.Genre
// 	item.Rating = nfo.Rating
// 	item.Votes = nfo.Votes
// 	if nfo.Year != 0 {
// 		item.Year = nfo.Year
// 	}
// 	updated = ftime > otime
// 	return
// }
