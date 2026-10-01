package service

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// uuidToString renders a pgtype.UUID in canonical lower-case 8-4-4-4-12 form,
// or "" when it is not valid.
func uuidToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
