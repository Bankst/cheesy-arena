package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestQuickPlay(t *testing.T) {
	web := setupTestWeb(t)

	recorder := web.getHttpResponse("/quickplay")
	assert.Equal(t, 200, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "Quick Play")
	assert.Contains(t, recorder.Body.String(), "Load &amp; Arm")
	assert.Contains(t, recorder.Body.String(), `id="qpRed1"`)
	assert.Contains(t, recorder.Body.String(), `id="qpBlue3"`)
}
