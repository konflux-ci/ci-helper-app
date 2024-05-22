// Copyright 2018 Palantir Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v58/github"
	reporters "github.com/onsi/ginkgo/v2/reporters"
	"github.com/palantir/go-githubapp/githubapp"
	"github.com/pkg/errors"
	"github.com/redhat-appstudio/qe-tools/pkg/prow"
	"github.com/rs/zerolog"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	targetAuthor             = "openshift-ci[bot]"
	junitFilename            = "junit.xml"
	junitFilenameRegex       = `(junit.xml)`
	openshiftCITestSuiteName = "openshift-ci job"
	e2eTestSuiteName         = "Red Hat App Studio E2E tests"
	LogKeyProwJobURL         = "prow_job_url"
	dropdownSummaryString    = "Click to view logs"
	CRsJunitPropertyName     = "redhat-appstudio-gather"
	podsJunitPropertyName    = "gather-extra"
	regexToFetchProwURL      = `(https:\/\/prow.ci.openshift.org\/view\/gs\/test-platform-results\/pr-logs\/pull.*)\)`
)

type PRCommentHandler struct {
	githubapp.ClientCreator
}

type FailedTestCasesReport struct {
	headerString        string
	podsLink            string
	failedTestCaseNames []string
	hasBootstrapFailure bool
	customResourcesLink string
}

func (h *PRCommentHandler) Handles() []string {
	return []string{"issue_comment"}
}

func (h *PRCommentHandler) Handle(ctx context.Context, eventType, deliveryID string, payload []byte) error {
	var event github.IssueCommentEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return errors.Wrap(err, "failed to parse issue comment event payload")
	}

	if !event.GetIssue().IsPullRequest() || event.GetAction() != "created" {
		return nil
	}

	installationID := githubapp.GetInstallationIDFromEvent(&event)

	ctx, logger := githubapp.PreparePRContext(ctx, installationID, event.GetRepo(), event.GetIssue().GetNumber())

	client, err := h.NewInstallationClient(installationID)
	if err != nil {
		return err
	}

	author := event.GetComment().GetUser().GetLogin()
	body := event.GetComment().GetBody()

	if !strings.HasPrefix(author, targetAuthor) {
		logger.Debug().Msgf("Issue comment was not created by the user: %s. Ignoring this comment", targetAuthor)
		return nil
	}

	// extract the Prow job's URL
	prowJobURL, err := extractProwJobURLFromCommentBody(body)
	if err != nil {
		return fmt.Errorf("unable to extract Prow job's URL from the PR comment's body: %+v", err)
	}

	logger = attachProwURLLogKeysToLogger(ctx, logger, prowJobURL)

	cfg := prow.ScannerConfig{
		ProwJobURL:     prowJobURL,
		FileNameFilter: []string{junitFilenameRegex},
	}

	scanner, err := prow.NewArtifactScanner(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize ArtifactScanner: %+v", err)
	}

	err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 10*time.Minute, true, func(context.Context) (done bool, err error) {
		if err := scanner.Run(); err != nil {
			logger.Error().Err(err).Msgf("Failed to scan artifacts from the Prow job...Retrying")
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		logger.Error().Err(err).Msgf("Timed out while scanning artifacts for Prow job %s. Will Stop processing this comment", prowJobURL)
		return err
	}

	overallJUnitSuites, err := getTestSuitesFromXMLFile(scanner, logger, junitFilename)
	// make sure that the Prow job didn't fail while creating the cluster
	if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("couldn't find the %s file", junitFilename)) {
		return fmt.Errorf("failed to get JUnitTestSuites from the file %s: %+v", junitFilename, err)
	}

	failedTCReport := setHeaderString(logger, overallJUnitSuites)
	failedTCReport.extractFailedTestCases(scanner, logger, overallJUnitSuites)
	failedTCReport.initPodAndCRsLink(overallJUnitSuites)

	if err = failedTCReport.updateCommentWithFailedTestCasesReport(ctx, logger, client, event, body); err != nil {
		return err
	}

	return nil
}

// extractProwJobURLFromCommentBody extracts the
// Prow job's URL from the given PR comment's body
func extractProwJobURLFromCommentBody(commentBody string) (string, error) {
	r, _ := regexp.Compile(regexToFetchProwURL)
	sliceOfMatchingString := r.FindAllStringSubmatch(commentBody, -1)

	for _, matchesAndGroups := range sliceOfMatchingString {
		for _, subsStr := range matchesAndGroups {
			if !strings.Contains(subsStr, "images") && !strings.HasSuffix(subsStr, ")") {
				return subsStr, nil
			}
		}
	}

	return "", fmt.Errorf("regex string %s found no matches for the comment body: %s", regexToFetchProwURL, commentBody)
}

// getTestSuitesFromXMLFile returns all the JUnitTestSuites
// present within a file with the given name
func getTestSuitesFromXMLFile(scanner *prow.ArtifactScanner, logger zerolog.Logger, filename string) (*reporters.JUnitTestSuites, error) {
	overallJUnitSuites := &reporters.JUnitTestSuites{}

	for _, artifactsFilenameMap := range scanner.ArtifactStepMap {
		for artifactFilename, artifact := range artifactsFilenameMap {
			if string(artifactFilename) == filename {
				if err := xml.Unmarshal([]byte(artifact.Content), overallJUnitSuites); err != nil {
					logger.Error().Err(err).Msg("cannot decode JUnit suite into xml")
					return &reporters.JUnitTestSuites{}, err
				}
				return overallJUnitSuites, nil
			}
		}
	}

	return &reporters.JUnitTestSuites{}, fmt.Errorf("couldn't find the %s file", filename)
}

// setHeaderString initialises struct FailedTestCasesReport's
// 'headerString' field based on phase at which Prow job failed
func setHeaderString(logger zerolog.Logger, overallJUnitSuites *reporters.JUnitTestSuites) *FailedTestCasesReport {
	failedTCReport := FailedTestCasesReport{}

	if len(overallJUnitSuites.TestSuites) == 0 {
		logger.Debug().Msg("The given Prow job failed while creating the cluster")
		failedTCReport.headerString = ":rotating_light: **This is a CI system failure, please consult with the QE team.**\n"
	} else if len(overallJUnitSuites.TestSuites) == 1 && overallJUnitSuites.TestSuites[0].Name == openshiftCITestSuiteName {
		logger.Debug().Msg("The given Prow job failed during bootstrapping the cluster")
		failedTCReport.hasBootstrapFailure = true
		failedTCReport.headerString = ":rotating_light: **Error occurred during the cluster's Bootstrapping phase, list of failed Spec(s)**: \n"
	} else {
		logger.Debug().Msg("The given Prow job failed while running the E2E tests")
		failedTCReport.headerString = ":rotating_light: **Error occurred while running the E2E tests, list of failed Spec(s)**: \n"
	}

	return &failedTCReport
}

// initPodAndCRsLink initialises the FailedTestCasesReport struct's
// 'podsLink' and 'customResourcesLink' field with the link to the
// directory where pod logs and generated custom resources are
// stored, respectively.
func (failedTCReport *FailedTestCasesReport) initPodAndCRsLink(overallJUnitSuites *reporters.JUnitTestSuites) {
	for _, testSuite := range overallJUnitSuites.TestSuites {
		if testSuite.Name != openshiftCITestSuiteName {
			continue
		}

		foundCRsProperty := false
		foundPodsProperty := false

		for _, property := range testSuite.Properties.Properties {
			if property.Name == CRsJunitPropertyName {
				failedTCReport.customResourcesLink = property.Value
				foundCRsProperty = true
			}
			if property.Name == podsJunitPropertyName {
				failedTCReport.podsLink = property.Value
				foundPodsProperty = true
			}

			if foundCRsProperty && foundPodsProperty {
				break // Exit inner loop early if both properties are found
			}
		}

		break // Exit outer loop early once the 'openshiftCITestSuiteName' test suite is processed
	}
}

// extractFailedTestCases initialises the FailedTestCasesReport struct's
// 'failedTestCaseNames' field with the names of failed test cases
// within given JUnitTestSuites -- if the given JUnitTestSuites is !nil.
// And if it's nil, 'failedTestCaseNames' field is init with content of
// "build-log.txt" file, if it exists.
func (failedTCReport *FailedTestCasesReport) extractFailedTestCases(scanner *prow.ArtifactScanner, logger zerolog.Logger, overallJUnitSuites *reporters.JUnitTestSuites) {
	if len(overallJUnitSuites.TestSuites) == 0 {
		parentStepName := "/"
		buildLogFileName := "build-log.txt"

		if asMap := scanner.ArtifactStepMap[prow.ArtifactStepName(parentStepName)]; asMap != nil {
			if asMap[prow.ArtifactFilename(buildLogFileName)].Content == "" {
				logger.Error().Msgf("Failed to fetch content of the file: %s within the `%s` parent directory", buildLogFileName, parentStepName)
				return
			}

			testCaseEntry := returnContentWrappedInDropdown(dropdownSummaryString, asMap[prow.ArtifactFilename(buildLogFileName)].Content)
			failedTCReport.failedTestCaseNames = append(failedTCReport.failedTestCaseNames, testCaseEntry)
		} else {
			logger.Error().Msgf("Failed to find any files within the directory: %s", parentStepName)
		}
		return
	}

	for _, testSuite := range overallJUnitSuites.TestSuites {
		if failedTCReport.hasBootstrapFailure || (testSuite.Name == e2eTestSuiteName && (testSuite.Failures > 0 || testSuite.Errors > 0)) {
			for _, tc := range testSuite.TestCases {
				if tc.Failure != nil || tc.Error != nil {
					logger.Debug().Msgf("Found a Test Case (suiteName/testCaseName): %s/%s, that didn't pass", testSuite.Name, tc.Name)
					tcMessage := ""
					if failedTCReport.hasBootstrapFailure {
						tcMessage = "```\n" + returnLastNLines(tc.SystemErr, 16) + "\n```"
					} else if tc.Status == "timedout" {
						tcMessage = returnContentWrappedInDropdown(dropdownSummaryString, tc.SystemErr)
					} else if tc.Failure != nil {
						tcMessage = "```\n" + tc.Failure.Message + "\n```"
					} else {
						tcMessage = "```\n" + tc.Error.Message + "\n```"
					}
					testCaseEntry := "* :arrow_right: " + "[**`" + tc.Status + "`**] " + tc.Name + "\n" + tcMessage
					failedTCReport.failedTestCaseNames = append(failedTCReport.failedTestCaseNames, testCaseEntry)
				}
			}
		}
	}
}

// updateCommentWithFailedTestCasesReport updates the
// PR comment's body with the names of failed test cases
func (failedTCReport *FailedTestCasesReport) updateCommentWithFailedTestCasesReport(ctx context.Context, logger zerolog.Logger, client *github.Client, event github.IssueCommentEvent, commentBody string) error {
	repoOwner := event.GetRepo().GetOwner().GetLogin()
	repoName := event.GetRepo().GetName()
	commentID := event.GetComment().GetID()

	if failedTCReport.failedTestCaseNames != nil && len(failedTCReport.failedTestCaseNames) > 0 {
		msg := failedTCReport.headerString

		for _, failedTCName := range failedTCReport.failedTestCaseNames {
			msg = msg + fmt.Sprintf("\n %s\n", failedTCName)
		}

		if (failedTCReport.podsLink != "" && failedTCReport.customResourcesLink != "") {
			// Add pods and CRs' links
			msg = msg + fmt.Sprintf("[Link to Pod logs](%s). [Link to Custom Resources](%s)\n", failedTCReport.podsLink, failedTCReport.customResourcesLink)
		}

		msg = msg + "\n-------------------------------\n\n" + commentBody

		prComment := github.IssueComment{
			Body: &msg,
		}

		err := wait.PollUntilContextTimeout(context.Background(), 15*time.Second, 1*time.Minute, true, func(context.Context) (done bool, err error) {
			if _, _, err := client.Issues.EditComment(ctx, repoOwner, repoName, commentID, &prComment); err != nil {
				logger.Error().Err(err).Msgf("Failed to edit the comment...Retrying")
				return false, nil
			}

			return true, nil
		})
		if err != nil {
			logger.Error().Err(err).Msgf("Failed to edit comment (ID: %v) due to the error: %+v. Will Stop processing this comment", commentID, err)
			return err
		}

		logger.Debug().Msgf("Successfully updated comment (with ID:%d) with the names of failed test cases", commentID)
	} else {
		logger.Debug().Msgf("Unable to find any details to update. Declining to update comment (with ID:%d)", commentID)
	}

	return nil
}

func attachProwURLLogKeysToLogger(ctx context.Context, logger zerolog.Logger, prowJobURL string) zerolog.Logger {
	logctx := zerolog.Ctx(ctx).With()

	if prowJobURL != "" {
		logctx = logctx.Str(LogKeyProwJobURL, prowJobURL)
		return logctx.Logger()
	}
	return logger
}

func returnLastNLines(content string, n int) string {
	systemErrString := strings.Split(content, "\n")
	return strings.Join(systemErrString[len(systemErrString)-n:], "\n")
}

func returnContentWrappedInDropdown(summary, content string) string {
	return "<details><summary>" + summary + "</summary><br><pre>" + content + "</pre></details>"
}
