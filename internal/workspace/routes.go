package workspace

import (
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func (w *Service) executorOrMissing() (*executor, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.executor == nil {
		return nil, notFound("Executor isn't set up in the workspace config.")
	}
	return w.executor, nil
}

// Register serves the Files, Plugins and Executor pages under group.
func (w *Service) Register(group *gin.RouterGroup) {
	reply := func(c *gin.Context, value any, err error) {
		c.Header("Cache-Control", "no-store")
		if err != nil {
			status := statusOf(err)
			if status == http.StatusInternalServerError {
				log.Warnf("workspace: %s %s: %v", c.Request.Method, c.FullPath(), err)
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, value)
	}
	body := func(c *gin.Context, into any) error {
		if err := c.ShouldBindJSON(into); err != nil {
			return refuse("Bad request body: %v", err)
		}
		return nil
	}
	raw := func(c *gin.Context) ([]byte, error) {
		data, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBytes+1))
		if err == nil && len(data) > maxBytes {
			err = refuse("Over 2 MB, too large to save here.")
		}
		return data, err
	}
	ok := gin.H{"ok": true}

	group.GET("/repos", func(c *gin.Context) {
		repos, err := w.Repos()
		reply(c, repos, err)
	})
	group.POST("/repos/:repo/workspaces", func(c *gin.Context) {
		var in struct {
			Name string `json:"name"`
		}
		if err := body(c, &in); err != nil {
			reply(c, nil, err)
			return
		}
		id, err := w.Create(c.Param("repo"), in.Name)
		reply(c, gin.H{"id": id}, err)
	})
	group.POST("/repos/:repo/push", func(c *gin.Context) {
		reply(c, gin.H{"pushed": true}, w.Push(c.Param("repo")))
	})
	group.DELETE("/ws/:ws", func(c *gin.Context) {
		kept, err := w.Discard(c.Param("ws"))
		reply(c, gin.H{"branchKept": kept}, err)
	})
	group.GET("/ws/:ws/files", func(c *gin.Context) {
		files, err := w.Files(c.Param("ws"))
		reply(c, files, err)
	})
	group.GET("/ws/:ws/changes", func(c *gin.Context) {
		changes, err := w.Changes(c.Param("ws"))
		reply(c, changes, err)
	})
	group.GET("/ws/:ws/diff", func(c *gin.Context) {
		diff, err := w.Diff(c.Param("ws"))
		reply(c, diff, err)
	})
	group.GET("/ws/:ws/file", func(c *gin.Context) {
		file, err := w.Read(c.Param("ws"), c.Query("path"))
		reply(c, file, err)
	})
	group.GET("/ws/:ws/raw", func(c *gin.Context) {
		data, kind, err := w.Raw(c.Param("ws"), c.Query("path"))
		if err != nil {
			reply(c, nil, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Data(http.StatusOK, kind, data)
	})
	group.PUT("/ws/:ws/file", func(c *gin.Context) {
		data, err := raw(c)
		if err == nil {
			err = w.Write(c.Param("ws"), c.Query("path"), data)
		}
		reply(c, ok, err)
	})
	group.POST("/ws/:ws/commit", func(c *gin.Context) {
		var in struct {
			Message string `json:"message"`
		}
		if err := body(c, &in); err != nil {
			reply(c, nil, err)
			return
		}
		sha, signed, err := w.Commit(c.Param("ws"), in.Message)
		reply(c, gin.H{"sha": sha, "signed": signed}, err)
	})
	group.POST("/ws/:ws/ship", func(c *gin.Context) {
		shipped, err := w.Ship(c.Param("ws"))
		reply(c, shipped, err)
	})
	group.GET("/ws/:ws/commits", func(c *gin.Context) {
		limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
		if err != nil || limit < 1 {
			limit = 50
		}
		commits, err := w.Commits(c.Param("ws"), min(limit, 500), c.Query("path"))
		reply(c, commits, err)
	})
	group.GET("/ws/:ws/commits/:sha", func(c *gin.Context) {
		commit, err := w.CommitDetail(c.Param("ws"), c.Param("sha"))
		reply(c, commit, err)
	})

	group.GET("/drafts", func(c *gin.Context) {
		reply(c, w.Drafts(), nil)
	})
	group.GET("/drafts/:name", func(c *gin.Context) {
		contents, err := w.DraftContents(c.Param("name"))
		reply(c, gin.H{"contents": contents}, err)
	})

	group.GET("/plugins", func(c *gin.Context) {
		reply(c, w.Plugins(), nil)
	})
	group.POST("/plugins/:id", func(c *gin.Context) {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if err := body(c, &in); err != nil {
			reply(c, nil, err)
			return
		}
		reply(c, gin.H{"id": c.Param("id"), "enabled": in.Enabled}, w.SetPlugin(c.Param("id"), in.Enabled))
	})

	group.GET("/executor", func(c *gin.Context) {
		e, err := w.executorOrMissing()
		if err != nil {
			reply(c, nil, err)
			return
		}
		overview, err := e.overview.get()
		reply(c, overview, err)
	})
	group.GET("/executor/:slug", func(c *gin.Context) {
		e, err := w.executorOrMissing()
		if err != nil {
			reply(c, nil, err)
			return
		}
		files, err := e.Source(c.Param("slug"))
		reply(c, gin.H{"files": files}, err)
	})
	group.PUT("/executor/:slug/file", func(c *gin.Context) {
		e, err := w.executorOrMissing()
		var data []byte
		if err == nil {
			data, err = raw(c)
		}
		if err == nil {
			err = e.Save(c.Param("slug"), c.Query("path"), data)
		}
		reply(c, ok, err)
	})
	group.POST("/executor/:slug/deploy", func(c *gin.Context) {
		e, err := w.executorOrMissing()
		if err != nil {
			reply(c, nil, err)
			return
		}
		version, files, err := e.Deploy(c.Param("slug"))
		reply(c, gin.H{"version": version, "files": files}, err)
	})
	group.POST("/executor/:slug/connect", func(c *gin.Context) {
		e, err := w.executorOrMissing()
		var in struct {
			Requirement string `json:"requirement"`
		}
		if err == nil {
			err = body(c, &in)
		}
		var link string
		if err == nil {
			link, err = e.Connect(c.Param("slug"), in.Requirement)
		}
		reply(c, gin.H{"url": link}, err)
	})
}
