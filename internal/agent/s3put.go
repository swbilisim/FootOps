package agent

import (
	"strings"

	"github.com/swbilisim/FootOps/internal/s3obj"
)

// s3ObjectOf places name next to the repository: in its bucket and folder, with the
// repository's key (the signing itself is internal/s3obj's, which the Mac shares).
func s3ObjectOf(sec backupSecret, name string) (s3obj.Object, error) {
	if err := checkRepository(sec.Repository); err != nil {
		return s3obj.Object{}, err
	}
	return s3obj.At(strings.TrimPrefix(sec.Repository, "s3:"), name, sec.KeyID, sec.SecretKey)
}
