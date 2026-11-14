package jellyfin

import (
	"context"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

func (j *Jellyfin) userCanAccessCollection(ctx context.Context, userID, collectionID string) bool {
	if reqCtx, ok := ctx.Value(requestContextKey).(*requestContext); ok && reqCtx.User != nil && reqCtx.User.ID == userID {
		return canAccessCollection(reqCtx.User, collectionID)
	}
	user, err := j.repo.GetUserByID(ctx, userID)
	if err != nil {
		return false
	}
	return canAccessCollection(user, collectionID)
}

func canAccessCollection(user *model.User, collectionID string) bool {
	if user == nil || collectionID == "" || user.Properties.Disabled {
		return false
	}
	if user.Properties.Admin {
		return true
	}
	for _, id := range user.Properties.EnabledFolders {
		if id == collectionID {
			return true
		}
	}
	return false
}
