package pluginmarket

import (
	"archive/zip"
	"github.com/pelletier/go-toml/v2"
	"io"
	"strings"
)

// The catalog is not proof of the kind or identity inside an archive. A custom source may
// supply the namespace but cannot relabel an Agent binary as a Skill.
func VerifySkillArchive(path, id, version string) error {
	z, e := zip.OpenReader(path)
	if e != nil {
		return ErrPackage
	}
	defer z.Close()
	_, identifier, ok := strings.Cut(id, "/")
	if !ok {
		return ErrPackage
	}
	count := 0
	var manifest struct{ Kind, Identifier, Version string }
	for _, f := range z.File {
		if f.Name != "orax.toml" {
			continue
		}
		count++
		if count > 1 || f.UncompressedSize64 > maxManifestBytes || !f.Mode().IsRegular() {
			return ErrPackage
		}
		r, e := f.Open()
		if e != nil {
			return ErrPackage
		}
		data, e := io.ReadAll(io.LimitReader(r, maxManifestBytes+1))
		r.Close()
		if e != nil || len(data) > maxManifestBytes || toml.Unmarshal(data, &manifest) != nil {
			return ErrPackage
		}
	}
	if count != 1 || manifest.Kind != "skill" || manifest.Identifier != identifier || manifest.Version != version {
		return ErrPackage
	}
	return nil
}
