package management

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetPool returns the Claude account pool in rank order with its policy, limits, held resets and plans.
// session-cutoff, weekly-cutoff and agents-per-account rank it under proposed settings instead.
func (h *Handler) GetPool(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	var preview *config.PoolConfig
	if c.Query("session-cutoff") != "" {
		session, _ := strconv.ParseFloat(c.Query("session-cutoff"), 64)
		weekly, _ := strconv.ParseFloat(c.Query("weekly-cutoff"), 64)
		agents, _ := strconv.Atoi(c.Query("agents-per-account"))
		preview = &config.PoolConfig{SessionCutoff: session, WeeklyCutoff: weekly, AgentsPerAccount: agents}
	}
	c.JSON(http.StatusOK, h.authManager.PoolSnapshot(preview))
}

// GetPoolHistory returns the limit readings of the last `days` days (default 7).
func (h *Handler) GetPoolHistory(c *gin.Context) {
	days, err := strconv.Atoi(c.DefaultQuery("days", "7"))
	if err != nil || days < 1 || days > 90 {
		days = 7
	}
	c.JSON(http.StatusOK, gin.H{"readings": coreauth.PoolHistory(time.Now().AddDate(0, 0, -days).UnixMilli())})
}
