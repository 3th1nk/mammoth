package dd

// NoCloud seed rendering (roadmap「镜像直写通路」slice 2): identity/network/
// access intent lands as a cloud-init seed on a CIDATA partition the runtime
// appends to the freshly written image — the image boots its own bootloader,
// its cloud-init picks the seed up on first boot, and the spec's intent
// survives the dd transport. Shared implementation lives in render/nocloud
// (bootc consumes the same CIDATA contract); the dd names stay package-local
// so the dd call sites and tests don't churn.

import (
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/nocloud"
)

func seedIntent(in render.InstallInputs) bool { return nocloud.SeedIntent(in) }

func seedFiles(in render.InstallInputs) (map[string]string, error) { return nocloud.SeedFiles(in) }

func seedPlanVars(in render.InstallInputs) (string, error) { return nocloud.SeedPlanVars(in) }
