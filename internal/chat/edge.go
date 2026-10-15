// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"agw/internal/delegation"
	"agw/internal/platform"
)

// The edge guard and the provenance of images and deployments (edge D-E1, D1.7 and D1.8).
//
// The platform checks no owner on a deployment (db/models.py:196-208) and pulls every image with its
// own registry credentials (F1), so the gateway has to hold it: the agent touches only deployments of
// this chat, and a new deployment needs an image from this chat or from the user's own repository
// namespace. That holds with and without a delegation, because it is about what the platform would
// execute, not about what the user delegated.
func init() {
	registerGuard(func(ctx context.Context, m *Manager, chatID string, _ platform.Request, a delegation.Access) (string, error) {
		return m.edgeGuard(ctx, chatID, a)
	})
}

// The refusals of the guard (edge D-E1); tests and the plan quote them.
const (
	msgForeignDeployment = "deployment %s was not created in this chat; the agent may only start, undeploy or delete its own deployments"
	msgForeignImage      = "image %s may not be deployed: only images built in this chat or from your own repository namespace (%s/...)"
	msgTrainingImage     = "image %s is a training image, not an inference image"
	msgImageNotFound     = "image %s not found"
)

// edgeGuard holds with and without a delegation: deployments the agent did not create in this chat
// are out of reach, and a new deployment needs an image from this chat or from the user's own
// repository namespace (F1, F2). An empty string lets the call go on; an error fails closed.
func (m *Manager) edgeGuard(ctx context.Context, chatID string, a delegation.Access) (string, error) {
	if a.Resource != delegation.ContainerDeployment {
		return "", nil
	}
	if a.Action == delegation.Read {
		return "", nil
	}
	if a.Action != delegation.Create { // run (deploy, undeploy), delete
		own, err := m.st.IsDelegationObject(ctx, chatID, delegation.ContainerDeployment, a.ID)
		if err != nil {
			return "", err
		}
		if !own {
			return fmt.Sprintf(msgForeignDeployment, a.ID), nil
		}
		return "", nil
	}
	imageID := ""
	for _, x := range a.Also {
		if x.Resource == delegation.ContainerImage {
			imageID = x.ID
		}
	}
	img, err := m.imageByID(ctx, chatID, imageID) // reads the image list, logged as a read of the gateway
	if err != nil {
		return "", err
	}
	if img == nil {
		return fmt.Sprintf(msgImageNotFound, imageID), nil
	}
	ref := img.Repository + ":" + img.Tag
	if platform.IsTrainingImage(img.Tag, img.ExposedPorts) {
		return fmt.Sprintf(msgTrainingImage, ref), nil
	}
	if own, _ := m.st.IsDelegationObject(ctx, chatID, delegation.ContainerImage, ref); own {
		return "", nil
	}
	user, err := m.opt.Platform.ChatUser(ctx, chatID)
	if err != nil {
		return "", err
	}
	if user == "" || !strings.HasPrefix(img.Repository, user+"/") {
		return fmt.Sprintf(msgForeignImage, ref, user), nil
	}
	return "", nil
}

// imageByID reads the container image with this identifier from the platform (GET /container-images).
// It is a read of the gateway itself, not a call of the agent, and it is logged as such. A missing
// image answers nil: the guard tells the agent that image and id do not exist.
func (m *Manager) imageByID(ctx context.Context, chatID, id string) (*deploymentImage, error) {
	if id == "" {
		return nil, nil
	}
	res, err := m.opt.Platform.Do(ctx, chatID, platform.Request{Method: http.MethodGet, Path: "/container-images", Query: map[string]string{"limit": "1000"}})
	if err != nil {
		return nil, err
	}
	result := "ok"
	if res.Status != "ok" || res.HTTPStatus < 200 || res.HTTPStatus >= 300 {
		result = "error"
	}
	m.LogCallBy(ctx, m.slotOf(chatID), chatID, "orchestrator", "edge_image_read", "GET /container-images (ids)", result)
	if result == "error" {
		return nil, fmt.Errorf("image list not readable: %s", res.Message)
	}
	var images []deploymentImage
	if err := json.Unmarshal([]byte(res.Body), &images); err != nil {
		return nil, fmt.Errorf("image list unreadable: %w", err)
	}
	for i := range images {
		if images[i].ID.String() == id {
			return &images[i], nil
		}
	}
	return nil, nil
}

// deploymentImage is the part of a container image the guard reads. The identifier is a number in
// the platform's answer (`ContainerImage.id`), the repository the namespace of the image.
type deploymentImage struct {
	ID           json.Number `json:"id"`
	Repository   string      `json:"repository"`
	Tag          string      `json:"tag"`
	Platform     string      `json:"platform"`
	ExposedPorts []string    `json:"exposed_ports"`
}

// alwaysAttributed reports whether an object's provenance is recorded in every chat, with and without
// a delegation (edge D1.7): images, container deployments and the tasks of a deployment, whose guard
// needs them live. Every other object keeps the old rule and is recorded only with a delegation.
func alwaysAttributed(a delegation.Access) bool {
	if a.Resource == delegation.ContainerDeployment {
		return true // the deployment itself and the task of its deploy and undeploy
	}
	return a.Action == delegation.Create && a.Resource == delegation.ContainerImage
}

// recordDelegationObject adds an object to this chat's provenance register. A failure is logged, not
// returned: the call itself went through.
func (m *Manager) recordDelegationObject(ctx context.Context, chatID, resource, id string) {
	if err := m.st.AddDelegationObject(context.WithoutCancel(ctx), chatID, resource, id); err != nil {
		slog.Error("provenance not recorded", "chat", chatID, "object", resource+" "+id, "error", err)
	}
}
