package backup

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var managedBackupName = regexp.MustCompile(`^router-(\d{8}T\d{6}\.\d{9}Z)\.db$`)

type retainedBackup struct {
	path  string
	stamp time.Time
}

// ApplyRetention removes managed backups exceeding either configured bound.
// A zero limit disables that bound; the newest valid backup is always retained.
func ApplyRetention(dir string, maxAge time.Duration, maxCount int, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var files []retainedBackup
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		m := managedBackupName.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		stamp, err := time.Parse("20060102T150405.000000000Z", m[1])
		if err != nil {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if validate(path) != nil || validateSchema(path) != nil {
			continue
		}
		files = append(files, retainedBackup{path, stamp})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].stamp.After(files[j].stamp) })
	for i, f := range files {
		if i == 0 {
			continue
		}
		if (maxAge > 0 && now.Sub(f.stamp) > maxAge) || (maxCount > 0 && i >= maxCount) {
			if err := os.Remove(f.path); err != nil {
				return err
			}
		}
	}
	return nil
}
