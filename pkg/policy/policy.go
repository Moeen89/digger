package policy

import (
	"context"
	"digger/pkg/ci"
	"errors"
	"fmt"
	"github.com/open-policy-agent/opa/rego"
	"io"
	"net/http"
	"strings"
)

type PolicyProvider interface {
	GetPolicy(namespace string, projectname string) (string, error)
}

type DiggerHttpPolicyProvider struct {
	DiggerHost         string
	DiggerOrganisation string
	AuthToken          string
	HttpClient         *http.Client
}

type NoOpPolicyChecker struct {
}

func (p NoOpPolicyChecker) Check(_ string, _ string, _ string, _ string, _ string) (bool, error) {
	return true, nil
}

func (p *DiggerHttpPolicyProvider) getPolicyForOrganisation() (string, *http.Response, error) {
	organisation := p.DiggerOrganisation
	req, err := http.NewRequest("GET", p.DiggerHost+"/orgs/"+organisation+"/access-policy", nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Add("Authorization", "Bearer "+p.AuthToken)

	resp, err := p.HttpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp, nil
	}
	return string(body), resp, nil
}

func (p *DiggerHttpPolicyProvider) getPolicyForNamespace(namespace string, projectName string) (string, *http.Response, error) {

	// fetch RBAC policies for projectfrom Digger API
	namespace = strings.ReplaceAll(namespace, "/", "-")
	req, err := http.NewRequest("GET", p.DiggerHost+"/repos/"+namespace+"/projects/"+projectName+"/access-policy", nil)

	if err != nil {
		return "", nil, err
	}
	req.Header.Add("Authorization", "Bearer "+p.AuthToken)

	resp, err := p.HttpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp, nil
	}
	return string(body), resp, nil

}

// GetPolicy fetches policy for particular project,  if not found then it will fallback to org level policy
func (p *DiggerHttpPolicyProvider) GetPolicy(namespace string, projectName string) (string, error) {
	content, resp, err := p.getPolicyForNamespace(namespace, projectName)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == 200 {
		return content, nil
	} else if resp.StatusCode == 404 {
		content, resp, err := p.getPolicyForOrganisation()
		if err != nil {
			return "", err
		}
		if resp.StatusCode == 200 {
			return content, nil
		} else if resp.StatusCode == 404 {
			return "", nil
		} else {
			return "", errors.New(fmt.Sprintf("unexpected response while fetching organisation policy: %v, code %v", content, resp.StatusCode))
		}
	} else {
		return "", errors.New(fmt.Sprintf("unexpected response while fetching org policy: %v code %v", content, resp.StatusCode))
	}
}

type DiggerPolicyChecker struct {
	PolicyProvider DiggerHttpPolicyProvider
	ciService      ci.CIService
}

func (p DiggerPolicyChecker) Check(githubOrganisation string, namespace string, projectName string, command string, requestedBy string) (bool, error) {
	organisation := p.PolicyProvider.DiggerOrganisation
	policy, err := p.PolicyProvider.GetPolicy(namespace, projectName)
	teams, err := p.ciService.GetUserTeams(githubOrganisation, requestedBy)
	if err != nil {
		fmt.Printf("Error while fetching user teams for CI service: %v", err)
		return false, err
	}

	input := map[string]interface{}{
		"user":         requestedBy,
		"organisation": organisation,
		"teams":        teams,
		"action":       command,
		"project":      projectName,
	}

	if policy == "" {
		return true, nil
	}

	ctx := context.Background()
	fmt.Printf("DEBUG: passing the following input policy: %v ||| text: %v", input, policy)
	query, err := rego.New(
		rego.Query("data.digger.allow"),
		rego.Module("digger", policy),
	).PrepareForEval(ctx)

	if err != nil {
		return false, err
	}

	results, err := query.Eval(ctx, rego.EvalInput(input))
	if len(results) == 0 || len(results[0].Expressions) == 0 {
		return false, fmt.Errorf("no result found")
	}

	expressions := results[0].Expressions

	for _, expression := range expressions {
		decision, ok := expression.Value.(bool)
		if !ok {
			return false, fmt.Errorf("decision is not a boolean")
		}
		if !decision {
			return false, nil
		}
	}

	return true, nil
}
