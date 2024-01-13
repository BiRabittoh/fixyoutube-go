package invidious

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

const dbConnectionString = "file:cache.sqlite?cache=shared&mode="

func getDb(mode string) *sql.DB {
	db, err := sql.Open("sqlite3", dbConnectionString+mode)
	if err != nil {
		logger.Error("Could not open DB:", err)
		return nil
	}
	db.SetMaxOpenConns(1)
	return db
}

func InitDB() {
	db := getDb("rwc")
	defer db.Close()

	_, err := db.Exec(createQueryVideos)
	if err != nil {
		logger.Errorf("%q: %s\n", err, createQueryVideos)
		return
	}
	_, err = db.Exec(createQueryFormats)
	if err != nil {
		logger.Errorf("%q: %s\n", err, createQueryFormats)
		return
	}
}

func CacheVideoDB(v Video) error {
	db := getDb("rw")
	defer db.Close()

	cacheVideo, err := db.Prepare(cacheVideoQuery)
	if err != nil {
		logger.Error("Could not cache video: ", err)
		return err
	}
	defer cacheVideo.Close()

	_, err = cacheVideo.Exec(v.VideoId, v.Title, v.Description, v.Uploader, v.Duration, v.Expire)
	if err != nil {
		logger.Error("Could not cache video: ", err)
		return err
	}

	for _, f := range v.Formats {
		cacheFormat, err := db.Prepare(cacheFormatQuery)
		if err != nil {
			logger.Error("Could not cache format: ", err)
			return err
		}
		defer cacheVideo.Close()

		_, err = cacheFormat.Exec(v.VideoId, f.Name, f.Height, f.Width, f.Url)
		if err != nil {
			logger.Error("Could not cache format: ", err)
			return err
		}
	}
	return nil
}

func GetVideoDB(videoId string) (*Video, error) {
	db := getDb("ro")
	defer db.Close()

	getVideo, err := db.Prepare(getVideoQuery)
	if err != nil {
		logger.Error("Could not get video: ", err)
		return nil, err
	}
	defer getVideo.Close()

	v := &Video{}
	err = getVideo.QueryRow(videoId).Scan(&v.VideoId, &v.Title, &v.Description, &v.Uploader, &v.Duration, &v.Timestamp, &v.Expire)
	if err != nil {
		logger.Debug("Could not get video:", err)
		return nil, err
	}

	if v.Timestamp.After(v.Expire) {
		logger.Info("Video has expired.")
		return nil, fmt.Errorf("expired")
	}

	getFormat, err := db.Prepare(getFormatQuery)
	if err != nil {
		logger.Error("Could not get format: ", err)
		return nil, err
	}
	defer getFormat.Close()

	response, err := getFormat.Query(videoId)
	if err != nil {
		logger.Error("Could not get formats: ", err)
		return nil, err
	}
	defer response.Close()

	for response.Next() {
		f := Format{}
		err := response.Scan(&f.VideoId, &f.Name, &f.Height, &f.Width, &f.Url)
		if err != nil {
			logger.Error("Could not get formats: ", err)
			return nil, err
		}
		v.Formats = append(v.Formats, f)
	}

	return v, nil
}

func ClearDB() {
	db := getDb("rw")
	defer db.Close()

	stmt, err := db.Prepare(clearQuery)
	if err != nil {
		logger.Error("Could not clear DB:", err)
		return
	}
	defer stmt.Close()

	_, err = stmt.Exec()
	if err != nil {
		logger.Error("Could not clear DB:", err)
		return
	}
}
