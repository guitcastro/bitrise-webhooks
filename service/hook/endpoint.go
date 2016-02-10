package hook

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"

	"github.com/bitrise-io/bitrise-webhooks/bitriseapi"
	"github.com/bitrise-io/bitrise-webhooks/config"
	"github.com/bitrise-io/bitrise-webhooks/metrics"
	"github.com/bitrise-io/bitrise-webhooks/service"
	hookCommon "github.com/bitrise-io/bitrise-webhooks/service/hook/common"
	"github.com/bitrise-io/bitrise-webhooks/service/hook/github"
	"github.com/gorilla/mux"
)

func supportedProviders() map[string]hookCommon.Provider {
	return map[string]hookCommon.Provider{
		"github": github.HookProvider{},
		// "bitbucket-v2": bitbucketv2.HookProvider{},
	}
}

// SuccessRespModel ...
type SuccessRespModel struct {
	Message string `json:"message"`
}

// ErrorRespModel ...
type ErrorRespModel struct {
	Errors []error `json:"errors"`
}

func respondWithSingleError(w http.ResponseWriter, err error) {
	respondWithErrors(w, []error{err})
}

func respondWithSingleErrorStr(w http.ResponseWriter, errStr string) {
	respondWithSingleError(w, errors.New(errStr))
}

func respondWithErrors(w http.ResponseWriter, errs []error) {
	service.RespondWithErrorJSON(w, http.StatusBadRequest, ErrorRespModel{Errors: errs})
}

func triggerBuild(triggerURL *url.URL, apiToken string, triggerAPIParams bitriseapi.TriggerAPIParamsModel) error {
	isOnlyLog := !(config.SendRequestToURL != nil || config.GetServerEnvMode() == config.ServerEnvModeProd)

	_, err := bitriseapi.TriggerBuild(triggerURL, apiToken, triggerAPIParams, isOnlyLog)
	if err != nil {
		return fmt.Errorf("Failed to Trigger the Build: %s", err)
	}
	return nil
}

// HTTPHandler ...
func HTTPHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serviceID := vars["service-id"]
	appSlug := vars["app-slug"]
	apiToken := vars["api-token"]

	if serviceID == "" {
		respondWithSingleErrorStr(w, "No service-id defined")
		return
	}
	if appSlug == "" {
		respondWithSingleErrorStr(w, "No App Slug parameter defined")
		return
	}
	if apiToken == "" {
		respondWithSingleErrorStr(w, "No API Token parameter defined")
		return
	}

	hookProvider, isSupported := supportedProviders()[serviceID]
	if !isSupported {
		respondWithSingleErrorStr(w, fmt.Sprintf("Unsupported Webhook Type / Provider: %s", serviceID))
		return
	}

	hookTransformResult := hookCommon.TransformResultModel{}
	metrics.Trace("Hook: Transform", func() {
		hookTransformResult = hookProvider.Transform(r)
	})

	if hookTransformResult.ShouldSkip {
		resp := SuccessRespModel{
			Message: fmt.Sprintf("Acknowledged, but skipping. Reason: %s", hookTransformResult.Error),
		}
		service.RespondWithSuccess(w, resp)
		return
	}
	if hookTransformResult.Error != nil {
		errMsg := fmt.Sprintf("Failed to transform the webhook: %s", hookTransformResult.Error)
		log.Printf(" (debug) %s", errMsg)
		respondWithSingleErrorStr(w, errMsg)
		return
	}

	// Let's Trigger a Build!
	triggerURL := config.SendRequestToURL
	if triggerURL == nil {
		u, err := bitriseapi.BuildTriggerURL("https://www.bitrise.io", appSlug)
		if err != nil {
			log.Printf(" [!] Exception: hookHandler: failed to create Build Trigger URL: %s", err)
			respondWithSingleErrorStr(w, fmt.Sprintf("Failed to create Build Trigger URL: %s", err))
			return
		}
		triggerURL = u
	}

	respondWithErrors := []error{}
	buildTriggerCount := len(hookTransformResult.TriggerAPIParams)
	metrics.Trace("Hook: Trigger Builds", func() {
		if buildTriggerCount == 0 {
			respondWithErrors = append(respondWithErrors, errors.New("After processing the webhook we failed to detect any event in it which could be turned into a build."))
			return
		} else if buildTriggerCount == 1 {
			err := triggerBuild(triggerURL, apiToken, hookTransformResult.TriggerAPIParams[0])
			if err != nil {
				respondWithErrors = append(respondWithErrors, err)
				return
			}
		} else {
			for _, aBuildTriggerParam := range hookTransformResult.TriggerAPIParams {
				if err := triggerBuild(triggerURL, apiToken, aBuildTriggerParam); err != nil {
					respondWithErrors = append(respondWithErrors, err)
				}
			}
		}
	})

	if len(respondWithErrors) > 0 {
		service.RespondWithErrorJSON(w, http.StatusBadRequest, ErrorRespModel{Errors: respondWithErrors})
		return
	}

	successMsg := ""
	if buildTriggerCount == 1 {
		successMsg = "Successfully triggered 1 build."
	} else {
		successMsg = fmt.Sprintf("Successfully triggered %d builds.", buildTriggerCount)
	}
	service.RespondWithSuccess(w, SuccessRespModel{Message: successMsg})
}
