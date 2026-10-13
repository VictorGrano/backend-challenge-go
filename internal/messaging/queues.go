package messaging

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func NewClient(ctx context.Context) (*sqs.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("messaging: load aws config: %w", err)
	}

	if cfg.Region == "" {
		return nil, errors.New("messaging: AWS_REGION is required")
	}

	return sqs.NewFromConfig(cfg), nil
}

type QueueNames struct {
	Input      string
	DeadLetter string
	Events     string
}

type Queues struct {
	client *sqs.Client
	names  QueueNames

	mu   sync.RWMutex
	urls map[string]string
}

func NewQueues(client *sqs.Client, names QueueNames) (*Queues, error) {
	if names.Input == "" || names.DeadLetter == "" || names.Events == "" {
		return nil, errors.New("messaging: queue names are required")
	}

	return &Queues{client: client, names: names, urls: map[string]string{}}, nil
}

func (q *Queues) Resolve(ctx context.Context) error {
	for _, name := range []string{q.names.Input, q.names.DeadLetter, q.names.Events} {
		out, err := q.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err != nil {
			return fmt.Errorf("messaging: resolve queue %s: %w", name, err)
		}

		q.mu.Lock()
		q.urls[name] = aws.ToString(out.QueueUrl)
		q.mu.Unlock()
	}

	return nil
}

func (q *Queues) url(name string) string {
	q.mu.RLock()
	defer q.mu.RUnlock()

	return q.urls[name]
}

func (q *Queues) InputURL() string      { return q.url(q.names.Input) }
func (q *Queues) DeadLetterURL() string { return q.url(q.names.DeadLetter) }
func (q *Queues) EventsURL() string     { return q.url(q.names.Events) }

func (q *Queues) Ping(ctx context.Context) error {
	_, err := q.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(q.names.Input)})
	if err != nil {
		return fmt.Errorf("messaging: sqs unavailable: %w", err)
	}

	return nil
}
