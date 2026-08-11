package admission

import "fmt"

// Key builders for every admission Redis key.
// All keys are prefixed with "nexus:{admission}:" to document that they live
// on the dedicated admission Redis instance.

func keyModelRPM(modelName string) string {
	return fmt.Sprintf("nexus:{admission}:model:%s:rpm", modelName)
}

func keyModelITPM(modelName string) string {
	return fmt.Sprintf("nexus:{admission}:model:%s:itpm", modelName)
}

func keyModelOTPM(modelName string) string {
	return fmt.Sprintf("nexus:{admission}:model:%s:otpm", modelName)
}

func keyProjectRPM(projectID string) string {
	return fmt.Sprintf("nexus:{admission}:project:%s:rpm", projectID)
}

func keyProjectITPM(projectID string) string {
	return fmt.Sprintf("nexus:{admission}:project:%s:itpm", projectID)
}

func keyProjectInflight(projectID string) string {
	return fmt.Sprintf("nexus:{admission}:project:%s:inflight", projectID)
}

// keyProjectDaily encodes the admissionDate (YYYY-MM-DD UTC) in the key
// so late corrections apply to the correct quota window.
func keyProjectDaily(projectID, admissionDate string) string {
	return fmt.Sprintf("nexus:{admission}:project:%s:daily:%s", projectID, admissionDate)
}

func keyProjectMonthly(projectID, admissionMonth string) string {
	return fmt.Sprintf("nexus:{admission}:project:%s:monthly:%s", projectID, admissionMonth)
}

func keyTeamRPM(teamID string) string {
	return fmt.Sprintf("nexus:{admission}:team:%s:rpm", teamID)
}

func keyTeamITPM(teamID string) string {
	return fmt.Sprintf("nexus:{admission}:team:%s:itpm", teamID)
}

func keyTeamOTPM(teamID string) string {
	return fmt.Sprintf("nexus:{admission}:team:%s:otpm", teamID)
}

func keyOrgMonthly(orgID string) string {
	return fmt.Sprintf("nexus:{admission}:org:%s:monthly", orgID)
}

// keyToken holds the admission token UUID for ownership verification.
// TTL = billing authorization expires_at duration.
func keyToken(requestID string) string {
	return fmt.Sprintf("nexus:{admission}:token:%s", requestID)
}
