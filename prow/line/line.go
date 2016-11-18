/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package line

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/satori/go.uuid"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"
)

// Cut off line jobs after 10 hours.
const jobDeadline = 10 * time.Hour

type startClient interface {
	CreateJob(kube.Job) (kube.Job, error)
}

func StartJob(k *kube.Client, jobName string, pr github.PullRequest) error {
	lineImage := os.Getenv("LINE_IMAGE")
	if lineImage == "" {
		return errors.New("LINE_IMAGE not set")
	}
	dry, err := strconv.ParseBool(os.Getenv("DRY_RUN"))
	if err != nil {
		return fmt.Errorf("DRY_RUN not parseable: %v", err)
	}
	return startJob(k, jobName, pr, lineImage, dry)
}

func startJob(k startClient, jobName string, pr github.PullRequest, lineImage string, dry bool) error {
	name := uuid.NewV1().String()
	job := kube.Job{
		Metadata: kube.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"owner":            pr.Base.Repo.Owner.Login,
				"repo":             pr.Base.Repo.Name,
				"pr":               strconv.Itoa(pr.Number),
				"jenkins-job-name": jobName,
			},
			Annotations: map[string]string{
				"state":       "triggered",
				"author":      pr.User.Login,
				"description": "Build triggered.",
				"url":         "",
				"base-ref":    pr.Base.Ref,
				"base-sha":    pr.Base.SHA,
				"pull-sha":    pr.Head.SHA,
			},
		},
		Spec: kube.JobSpec{
			ActiveDeadlineSeconds: int(jobDeadline / time.Second),
			Template: kube.PodTemplateSpec{
				Spec: kube.PodSpec{
					RestartPolicy: "Never",
					Containers: []kube.Container{
						{
							Name:  "line",
							Image: os.Getenv("LINE_IMAGE"),
							Args: []string{
								"--job-name=" + jobName,
								"--repo-owner=" + pr.Base.Repo.Owner.Login,
								"--repo-name=" + pr.Base.Repo.Name,
								"--pr=" + strconv.Itoa(pr.Number),
								"--base-ref=" + pr.Base.Ref,
								"--base-sha=" + pr.Base.SHA,
								"--pull-sha=" + pr.Head.SHA,
								"--dry-run=" + strconv.FormatBool(dry),
								"--jenkins-url=$(JENKINS_URL)",
							},
							VolumeMounts: []kube.VolumeMount{
								{
									Name:      "oauth",
									ReadOnly:  true,
									MountPath: "/etc/github",
								},
								{
									Name:      "jenkins",
									ReadOnly:  true,
									MountPath: "/etc/jenkins",
								},
								{
									Name:      "labels",
									ReadOnly:  true,
									MountPath: "/etc/labels",
								},
								{
									Name:      "job-configs",
									ReadOnly:  true,
									MountPath: "/etc/jobs",
								},
							},
							Env: []kube.EnvVar{
								{
									Name: "JENKINS_URL",
									ValueFrom: &kube.EnvVarSource{
										ConfigMap: kube.ConfigMapKeySelector{
											Name: "jenkins-address",
											Key:  "jenkins-address",
										},
									},
								},
							},
						},
					},
					Volumes: []kube.Volume{
						{
							Name: "labels",
							DownwardAPI: &kube.DownwardAPISource{
								Items: []kube.DownwardAPIFile{
									{
										Path: "labels",
										Field: kube.ObjectFieldSelector{
											FieldPath: "metadata.labels",
										},
									},
								},
							},
						},
						{
							Name: "oauth",
							Secret: &kube.SecretSource{
								Name: "oauth-token",
							},
						},
						{
							Name: "jenkins",
							Secret: &kube.SecretSource{
								Name: "jenkins-token",
							},
						},
						{
							Name: "job-configs",
							ConfigMap: &kube.ConfigMapSource{
								Name: "job-configs",
							},
						},
					},
				},
			},
		},
	}
	if _, err := k.CreateJob(job); err != nil {
		return err
	}
	return nil
}

type deleteClient interface {
	ListJobs(labels map[string]string) ([]kube.Job, error)
	GetJob(name string) (kube.Job, error)
	PatchJob(name string, job kube.Job) (kube.Job, error)
	PatchJobStatus(name string, job kube.Job) (kube.Job, error)
}

func DeleteJob(k *kube.Client, jobName string, pr github.PullRequest) error {
	return deleteJob(k, jobName, pr)
}

func deleteJob(k deleteClient, jobName string, pr github.PullRequest) error {
	jobs, err := k.ListJobs(map[string]string{
		"owner":            pr.Base.Repo.Owner.Login,
		"repo":             pr.Base.Repo.Name,
		"pr":               strconv.Itoa(pr.Number),
		"jenkins-job-name": jobName,
	})
	if err != nil {
		return err
	}
	// Retry on conflict. This can happen if the job finishes and updates its
	// state right when we want to delete it.
	var job kube.Job
	for _, j := range jobs {
		job = j
		for i := 0; i < 3; i++ {
			if err := deleteKubeJob(k, job); err == nil {
				break
			} else {
				if _, ok := err.(kube.ConflictError); !ok {
					return err
				}
			}
			job, err = k.GetJob(j.Metadata.Name)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func deleteKubeJob(k deleteClient, job kube.Job) error {
	if job.Spec.Parallelism != nil && *job.Spec.Parallelism == 0 {
		// Already aborted this one.
		return nil
	} else if job.Status.Succeeded > 0 {
		// Already finished.
		return nil
	}
	// Delete the old job's pods by setting its parallelism to 0.
	parallelism := 0
	newStatus := job.Status
	newStatus.CompletionTime = time.Now()
	newAnnotations := job.Metadata.Annotations
	if newAnnotations == nil {
		newAnnotations = make(map[string]string)
	}
	newAnnotations["state"] = "aborted"
	newAnnotations["description"] = "Build aborted."
	newJob := kube.Job{
		Metadata: kube.ObjectMeta{
			Annotations: newAnnotations,
		},
		Spec: kube.JobSpec{
			Parallelism: &parallelism,
		},
		Status: newStatus,
	}
	// For some reason kubernetes makes you do this in two steps.
	if _, err := k.PatchJob(job.Metadata.Name, newJob); err != nil {
		return err
	}
	if _, err := k.PatchJobStatus(job.Metadata.Name, newJob); err != nil {
		return err
	}
	return nil
}

func SetJobStatus(k *kube.Client, jobName, state, desc, url string) error {
	j, err := k.GetJob(jobName)
	if err != nil {
		return err
	}
	newAnnotations := j.Metadata.Annotations
	if newAnnotations == nil {
		newAnnotations = make(map[string]string)
	}
	newAnnotations["state"] = state
	newAnnotations["description"] = desc
	newAnnotations["url"] = url
	_, err = k.PatchJob(jobName, kube.Job{
		Metadata: kube.ObjectMeta{
			Annotations: newAnnotations,
		},
	})
	return err
}