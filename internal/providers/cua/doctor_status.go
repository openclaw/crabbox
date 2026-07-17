package cua

func aggregateStatus(checks []DoctorCheck) string {
	for _, check := range checks {
		if check.Status == "failed" || check.Status == "missing" {
			return "failed"
		}
	}
	for _, check := range checks {
		if check.Status == "warning" {
			return "warning"
		}
	}
	return "ok"
}
