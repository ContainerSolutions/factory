package github

import "encoding/json"

// The parts of GitHub's webhook payloads the normaliser reads
// (https://docs.github.com/en/webhooks/webhook-events-and-payloads). Everything else stays in
// Event.Data.Raw.

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghRepository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

type ghIssue struct {
	Number            int             `json:"number"`
	Title             string          `json:"title"`
	Body              *string         `json:"body"`
	Labels            []ghLabel       `json:"labels"`
	User              ghUser          `json:"user"`
	AuthorAssociation string          `json:"author_association"`
	PullRequest       json.RawMessage `json:"pull_request"`
}

type ghComment struct {
	ID                int64  `json:"id"`
	Body              string `json:"body"`
	User              ghUser `json:"user"`
	AuthorAssociation string `json:"author_association"`
}

type ghPullRequest struct {
	Number int  `json:"number"`
	Draft  bool `json:"draft"`
	Merged bool `json:"merged"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User              ghUser `json:"user"`
	AuthorAssociation string `json:"author_association"`
}

type ghReview struct {
	ID                int64  `json:"id"`
	State             string `json:"state"`
	CommitID          string `json:"commit_id"`
	User              ghUser `json:"user"`
	AuthorAssociation string `json:"author_association"`
}

// ghChecks is a check_suite or a check_run: both carry a conclusion and the PRs they ran for.
type ghChecks struct {
	Conclusion   string `json:"conclusion"`
	PullRequests []struct {
		Number int `json:"number"`
		Base   struct {
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_requests"`
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
}

type ghDeployment struct {
	ID          int64  `json:"id"`
	SHA         string `json:"sha"`
	Environment string `json:"environment"`
}

type ghDeploymentStatus struct {
	State          string `json:"state"`
	Environment    string `json:"environment"`
	EnvironmentURL string `json:"environment_url"`
	TargetURL      string `json:"target_url"`
}

// envelope is a webhook payload: the action, who sent it, the repository and the event's
// object, whichever the event has.
type envelope struct {
	Action           string              `json:"action"`
	Sender           *ghUser             `json:"sender"`
	Repository       *ghRepository       `json:"repository"`
	Issue            *ghIssue            `json:"issue"`
	Comment          *ghComment          `json:"comment"`
	Label            *ghLabel            `json:"label"`
	PullRequest      *ghPullRequest      `json:"pull_request"`
	Review           *ghReview           `json:"review"`
	CheckSuite       *ghChecks           `json:"check_suite"`
	CheckRun         *ghChecks           `json:"check_run"`
	Release          *ghRelease          `json:"release"`
	Deployment       *ghDeployment       `json:"deployment"`
	DeploymentStatus *ghDeploymentStatus `json:"deployment_status"`
}
