package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/costexplorer"
	"github.com/aws/aws-sdk-go/service/kms"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type (
	cost struct {
		key    string
		amount float64
		unit   string
	}

	HandlerFunc func() error
)

func init() {
	log.SetFormatter(&log.JSONFormatter{})
}

func main() {
	switch {
	case isAWSLambdaEnv():
		lambda.Start(GetLambdaHandler())
	default:
		start()
	}
}

func isAWSLambdaEnv() bool {
	return os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != ""
}

func GetLambdaHandler() HandlerFunc {
	svc := kms.New(session.New(), aws.NewConfig().WithRegion("ap-northeast-1"))

	webhook, err := decodeKMS(svc, os.Getenv("WEBHOOK"))
	if err != nil {
		log.Fatal(err)
	}

	channel, err := decodeKMS(svc, os.Getenv("CHANNEL"))
	if err != nil {
		log.Fatal(err)
	}

	return func() error {
		return run(webhook, channel)
	}
}

func decodeKMS(svc *kms.KMS, data string) (string, error) {
	dataBytes, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", errors.Wrap(err, "failed to decode KMS data as Base64")
	}

	var in = &kms.DecryptInput{
		CiphertextBlob: dataBytes,
	}
	out, err := svc.Decrypt(in)
	if err != nil {
		return "", errors.Wrap(err, "failed to decrypt KMS value")
	}

	return string(out.Plaintext), nil
}

func start() {
	if err := run(os.Getenv("WEBHOOK"), os.Getenv("CHANNEL")); err != nil {
		log.Fatal(err)
	}
}

func run(webhook string, channel string) error {
	details, err := getCosts()
	if err != nil {
		return errors.Wrap(err, "failed to get cost")
	}

	if err := postSlack(webhook, channel, details); err != nil {
		return errors.Wrap(err, "failed to send cost into slack")
	}

	return nil
}

func getCosts() ([]cost, error) {
	costExplorer := costexplorer.New(session.New())

	now := time.Now().In(time.UTC)
	startDate := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	endDate := startDate.AddDate(0, 1, 0)
	dateFormat := "2006-01-02"
	in := costexplorer.GetCostAndUsageInput{
		TimePeriod: &costexplorer.DateInterval{
			Start: aws.String(startDate.Format(dateFormat)),
			End:   aws.String(endDate.Format(dateFormat)),
		},
		Metrics:     []*string{aws.String("UnblendedCost")},
		Granularity: aws.String("MONTHLY"),
		GroupBy: []*costexplorer.GroupDefinition{
			{
				Key:  aws.String("SERVICE"),
				Type: aws.String("DIMENSION"),
			},
		},
	}
	out, err := costExplorer.GetCostAndUsage(&in)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get cost and usage %+v", in)
	}
	log.WithFields(log.Fields{"in": in, "out": *out}).Info("cost and usage")

	costs := make([]cost, 0)
	for _, result := range out.ResultsByTime {
		for _, group := range result.Groups {
			key := ""
			if len(group.Keys) >= 1 {
				key = *group.Keys[0]
			}

			var unit, amount string
			metric := group.Metrics["UnblendedCost"]
			if metric != nil {
				if metric.Amount != nil {
					amount = *metric.Amount
				}
				if metric.Unit != nil {
					unit = *metric.Unit
				}
			}

			amountVal, err := strconv.ParseFloat(amount, 64)
			if err != nil {
				return nil, errors.Wrap(err, "failed to parse amount")
			}

			costs = append(costs, cost{
				key:    key,
				amount: amountVal,
				unit:   unit,
			})
		}
	}
	sort.Slice(costs, func(i, j int) bool {
		return costs[i].amount > costs[j].amount
	})

	total := float64(0)
	for _, c := range costs {
		total += c.amount
	}
	costs = append(
		[]cost{{key: "Total", amount: total, unit: "*"}},
		costs...,
	)

	return costs, nil
}

func postSlack(webhookURL string, channelName string, details []cost) error {
	texts := make([]string, 0)
	for _, detail := range details {
		key := strings.TrimSpace(
			strings.NewReplacer("AWS", "", "Amazon", "").Replace(detail.key),
		)
		unit := strings.TrimSpace(detail.unit)
		texts = append(texts, fmt.Sprintf("%-40s : %10.3f %s", key, detail.amount, unit))
	}
	text := fmt.Sprintf("```\n%s\n```", strings.Join(texts, "\n"))

	req := map[string]interface{}{
		"text":        "AWS Cost and Usage",
		"channelName": channelName,
		"attachments": []map[string]interface{}{
			{
				"text": text,
			},
		},
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return errors.Wrapf(err, "failed to serialize request. %+v", req)
	}

	resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(reqBytes))
	if err != nil {
		return errors.Wrap(err, "failed to send request")
	}

	respBodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read response body")
	}

	log.WithFields(log.Fields{"req body": req, "res body": respBodyBytes, "status": resp.Status}).Info("slack")

	if resp.StatusCode != http.StatusOK {
		return errors.New(fmt.Sprintf("invalid status %s", resp.Status))
	}

	return nil
}
