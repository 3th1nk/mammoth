// Vendored static assets for the console panel: noVNC (MPL-2.0, see
// assets/novnc/LICENSE.txt). The KVM page imports them under
// /bmcweb/assets/novnc/... — the embed keeps the engine a single binary.
package bmcweb

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed all:assets/novnc
var novncFS embed.FS

// assetPath is the URL prefix the KVM page imports noVNC from.
const assetPath = "/bmcweb/assets/novnc/"

// serveAssets writes one embedded asset; false keeps the caller's fallback
// when the path is outside the tree.
func (p *Proxy) ServeAssets(c *gin.Context, path string) bool {
	if !strings.HasPrefix(path, assetPath) {
		return false
	}
	name := strings.TrimPrefix(path, assetPath)
	if name == "" || strings.Contains(name, "..") {
		return false
	}
	data, err := fs.ReadFile(novncFS, "assets/novnc/"+name)
	if err != nil {
		http.NotFound(c.Writer, c.Request)
		return true
	}
	if strings.HasSuffix(name, ".js") {
		c.Header("Content-Type", "text/javascript; charset=utf-8")
	} else {
		c.Header("Content-Type", http.DetectContentType(data))
	}
	// gin presets the NoRoute status to 404 before the handler runs — the
	// engine-owned answers must override it explicitly.
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write(data)
	return true
}
