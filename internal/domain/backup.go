package domain

import "time"

// BackupFile is a saved backup of the cluster state on a node.
type BackupFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	TakenAt time.Time `json:"taken_at"`
}

// RestoreSummary says what a restore applied, and which records it left
// out because their data is not in the backup.
type RestoreSummary struct {
	TakenAt         time.Time `json:"taken_at"`
	Node            string    `json:"node"`
	Settings        int       `json:"settings"`
	Tokens          int       `json:"tokens"`
	Modules         int       `json:"modules"`
	ModuleData      int       `json:"module_data"`
	RouteUsers      int       `json:"route_users"`
	Runtimes        int       `json:"runtimes"`
	Users           int       `json:"users"`
	Admin           bool      `json:"admin"`
	SkippedBases    []string  `json:"skipped_bases"`
	SkippedBranches []string  `json:"skipped_branches"`
}
