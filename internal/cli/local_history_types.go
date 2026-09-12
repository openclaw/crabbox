package cli

type fileLocalHistoryPolicy struct {
	Local *struct {
		Enabled *bool `yaml:"enabled,omitempty"`
	} `yaml:"local,omitempty"`
}

// Local history is self-recorded evidence, never coordinator attestation.
type localHistoryRecord struct {
	ImageEvidence           *ImageEvidence     `json:"imageEvidence,omitempty"`
	Version                 int                `json:"schemaVersion"`
	ID                      string             `json:"id"`
	RecordingState          string             `json:"recordingState"`
	Source                  string             `json:"source"`
	CoordinatorState        string             `json:"coordinatorState"`
	CoordinatorRunID        string             `json:"coordinatorRunId,omitempty"`
	CoordinatorReference    string             `json:"coordinatorReference,omitempty"`
	Provider                string             `json:"provider"`
	LeaseID                 string             `json:"leaseId,omitempty"`
	Slug                    string             `json:"slug,omitempty"`
	Target                  string             `json:"target,omitempty"`
	CommandDisplay          string             `json:"commandDisplay"`
	CommandDisplayTruncated bool               `json:"commandDisplayTruncated,omitempty"`
	Phase                   string             `json:"phase"`
	StartedAt               string             `json:"startedAt"`
	UpdatedAt               string             `json:"updatedAt"`
	EndedAt                 string             `json:"endedAt,omitempty"`
	ExitCode                *int               `json:"exitCode,omitempty"`
	RunStatus               RunStatus          `json:"runStatus,omitempty"`
	ErrorKind               RunErrorKind       `json:"errorKind,omitempty"`
	SyncMs                  int64              `json:"syncMs"`
	CommandMs               int64              `json:"commandMs"`
	TotalMs                 int64              `json:"totalMs"`
	Diagnostic              string             `json:"diagnostic,omitempty"`
	DiagnosticTruncated     bool               `json:"diagnosticTruncated,omitempty"`
	CaptureScope            string             `json:"captureScope"`
	Stdout                  localHistoryStream `json:"stdout"`
	Stderr                  localHistoryStream `json:"stderr"`
	LogBytes                int                `json:"logBytes"`
	LogTruncated            bool               `json:"logTruncated"`
	LogFullSHA256           string             `json:"logFullSha256,omitempty"`
	LogSHA256               string             `json:"logSha256,omitempty"`
	ResultsSHA256           string             `json:"resultsSha256,omitempty"`
	ResultsState            string             `json:"resultsState"`
	ResultsDetailsTruncated bool               `json:"resultsDetailsTruncated,omitempty"`
	ResultsOmittedFiles     int                `json:"resultsOmittedFiles,omitempty"`
	ResultsOmittedFailures  int                `json:"resultsOmittedFailures,omitempty"`
	ResultsClippedFields    int                `json:"resultsClippedFields,omitempty"`
}

type localHistoryStream struct {
	Availability string `json:"availability"`
	Reason       string `json:"reason,omitempty"`
}

type localHistoryFilter struct {
	LeaseID string
	State   string
	Limit   int
}
