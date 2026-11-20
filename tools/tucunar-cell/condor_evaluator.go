package canarynode

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

type condorWindowReader interface {
	ReadWindow(context.Context, string, time.Time, time.Time) ([]Signal, error)
}

func evaluatorWindow(reader condorWindowReader, ctx context.Context, genericRules []ThresholdRule, analyticalRules []AnalyticalRule, now time.Time) ([]Signal, error) {
	lookback := lookbackWindow
	for _, rule := range genericRules {
		if window := time.Duration(rule.WindowSeconds) * time.Second; window > lookback {
			lookback = window
		}
	}
	for _, rule := range analyticalRules {
		window := time.Duration(rule.WindowSeconds+rule.BaselineWindowSeconds) * time.Second
		if window > lookback {
			lookback = window
		}
	}
	return reader.ReadWindow(ctx, "canary-evaluator", now.Add(-lookback), now)
}

func conversionAlertsFromSignals(signals []Signal, window time.Duration, threshold float64, now time.Time) []ConversionAlert {
	type counts struct{ started, success uint64 }
	groups := make(map[string]counts)
	since := now.Add(-window)
	for _, signal := range signals {
		if signal.Source != "shopify" || signal.Kind != "event" || signal.OccurredAt.Before(since) || !signal.OccurredAt.Before(now) {
			continue
		}
		count := groups[signal.TenantID]
		switch signal.Name {
		case "checkout_started":
			count.started++
		case "payment_success":
			count.success++
		}
		groups[signal.TenantID] = count
	}
	alerts := make([]ConversionAlert, 0, len(groups))
	for shopID, count := range groups {
		if count.started < defaultRuleMinSamples {
			continue
		}
		rate := float64(count.success) / float64(count.started)
		if rate < threshold {
			alerts = append(alerts, ConversionAlert{ShopID: shopID, CheckoutStarted: count.started, PaymentSuccess: count.success, ConversionRate: rate})
		}
	}
	sort.Slice(alerts, func(i, j int) bool {
		if alerts[i].ConversionRate != alerts[j].ConversionRate {
			return alerts[i].ConversionRate < alerts[j].ConversionRate
		}
		return alerts[i].ShopID < alerts[j].ShopID
	})
	return alerts
}

type condorRuleGroup struct {
	tenant string
	entity string
}

func ruleGroup(signal Signal, groupBy []string) condorRuleGroup {
	group := condorRuleGroup{tenant: signal.TenantID}
	if len(groupBy) > 1 {
		group.entity = signal.EntityID
	}
	return group
}

func thresholdAlertsFromSignals(signals []Signal, rule ThresholdRule, now time.Time) ([]ThresholdAlert, error) {
	rule, err := rule.Validate()
	if err != nil {
		return nil, err
	}
	type bucket struct{ values []float64 }
	buckets := make(map[condorRuleGroup]bucket)
	since := now.Add(-time.Duration(rule.WindowSeconds) * time.Second)
	for _, signal := range signals {
		if signal.Source != rule.Source || signal.Kind != rule.Kind || signal.Name != rule.Name || signal.OccurredAt.Before(since) || !signal.OccurredAt.Before(now) {
			continue
		}
		group := ruleGroup(signal, rule.GroupBy)
		item := buckets[group]
		item.values = append(item.values, signal.Value)
		buckets[group] = item
	}
	alerts := make([]ThresholdAlert, 0, len(buckets))
	for group, bucket := range buckets {
		if uint64(len(bucket.values)) < defaultRuleMinSamples {
			continue
		}
		observed, err := condorAggregate(rule.Aggregate, bucket.values)
		if err != nil {
			return nil, err
		}
		if thresholdMatches(rule.Operator, observed, rule.Threshold) {
			alerts = append(alerts, mapThresholdResult(rule, group.tenant, group.entity, observed))
		}
	}
	sort.Slice(alerts, func(i, j int) bool {
		if alerts[i].TenantID != alerts[j].TenantID {
			return alerts[i].TenantID < alerts[j].TenantID
		}
		return alerts[i].EntityID < alerts[j].EntityID
	})
	return alerts, nil
}

func analyticalAlertsFromSignals(signals []Signal, rule AnalyticalRule, now time.Time) ([]AnalyticalAlert, error) {
	rule, err := rule.Validate()
	if err != nil {
		return nil, err
	}
	recentSince := now.Add(-time.Duration(rule.WindowSeconds) * time.Second)
	baselineSince := recentSince.Add(-time.Duration(rule.BaselineWindowSeconds) * time.Second)
	type bucket struct {
		recentValues, baselineValues []float64
		numerator, denominator       uint64
		recentCount, baselineCount   uint64
	}
	buckets := make(map[condorRuleGroup]bucket)
	for _, signal := range signals {
		if signal.Source != rule.Source || signal.Kind != rule.Kind || signal.OccurredAt.Before(baselineSince) || !signal.OccurredAt.Before(now) {
			continue
		}
		group := ruleGroup(signal, rule.GroupBy)
		item := buckets[group]
		switch rule.Family {
		case RuleFamilyAbsence, RuleFamilyWindowChange:
			if signal.Name != rule.Name {
				continue
			}
			if signal.OccurredAt.Before(recentSince) {
				item.baselineValues = append(item.baselineValues, signal.Value)
				item.baselineCount++
			} else {
				item.recentValues = append(item.recentValues, signal.Value)
				item.recentCount++
				if rule.Family == RuleFamilyAbsence {
					item.baselineCount++
				}
			}
		case RuleFamilyRatio:
			if signal.OccurredAt.Before(recentSince) {
				continue
			}
			switch signal.Name {
			case rule.Numerator:
				item.numerator++
			case rule.Denominator:
				item.denominator++
			default:
				continue
			}
		}
		buckets[group] = item
	}
	alerts := make([]AnalyticalAlert, 0, len(buckets))
	for group, bucket := range buckets {
		var alert AnalyticalAlert
		alert.RuleID, alert.Family, alert.Source = rule.ID, rule.Family, rule.Source
		alert.TenantID, alert.EntityID = group.tenant, group.entity
		alert.Threshold, alert.Operator = rule.Threshold, rule.Operator
		alert.Window = time.Duration(rule.WindowSeconds) * time.Second
		switch rule.Family {
		case RuleFamilyAbsence:
			if bucket.baselineCount < rule.MinSamples || bucket.recentCount >= rule.MinSamples {
				continue
			}
			alert.ObservedValue = float64(bucket.recentCount)
			alert.BaselineValue = float64(bucket.baselineCount)
			alert.RecentSamples, alert.BaselineSamples = bucket.recentCount, bucket.baselineCount
		case RuleFamilyRatio:
			if bucket.denominator < rule.MinSamples {
				continue
			}
			alert.ObservedValue = float64(bucket.numerator) / float64(bucket.denominator)
			if !thresholdMatches(rule.Operator, alert.ObservedValue, rule.Threshold) {
				continue
			}
			alert.BaselineValue = float64(bucket.denominator)
			alert.RecentSamples, alert.BaselineSamples = bucket.numerator, bucket.denominator
			alert.NumeratorSamples, alert.DenominatorSamples = bucket.numerator, bucket.denominator
		case RuleFamilyWindowChange:
			if bucket.baselineCount < rule.MinSamples {
				continue
			}
			recentValue, err := condorAggregate(rule.Aggregate, bucket.recentValues)
			if err != nil {
				return nil, err
			}
			baselineValue, err := condorAggregate(rule.Aggregate, bucket.baselineValues)
			if err != nil {
				return nil, err
			}
			if baselineValue == 0 {
				continue
			}
			alert.ObservedValue, alert.BaselineValue = recentValue/baselineValue, baselineValue
			alert.RecentSamples, alert.BaselineSamples = bucket.recentCount, bucket.baselineCount
			if !thresholdMatches(rule.Operator, alert.ObservedValue, rule.Threshold) {
				continue
			}
		}
		alerts = append(alerts, alert)
	}
	sort.Slice(alerts, func(i, j int) bool {
		if alerts[i].TenantID != alerts[j].TenantID {
			return alerts[i].TenantID < alerts[j].TenantID
		}
		return alerts[i].EntityID < alerts[j].EntityID
	})
	return alerts, nil
}

func thresholdMatches(operator ThresholdOperator, observed, threshold float64) bool {
	switch operator {
	case OperatorGT:
		return observed > threshold
	case OperatorGTE:
		return observed >= threshold
	case OperatorLT:
		return observed < threshold
	case OperatorLTE:
		return observed <= threshold
	default:
		return false
	}
}

func condorAggregate(aggregate string, values []float64) (float64, error) {
	if len(values) == 0 {
		return 0, nil
	}
	switch aggregate {
	case AggregateAverage:
		var total float64
		for _, value := range values {
			total += value
		}
		return total / float64(len(values)), nil
	case AggregateSum:
		var total float64
		for _, value := range values {
			total += value
		}
		return total, nil
	case AggregateMin:
		result := values[0]
		for _, value := range values[1:] {
			result = math.Min(result, value)
		}
		return result, nil
	case AggregateMax:
		result := values[0]
		for _, value := range values[1:] {
			result = math.Max(result, value)
		}
		return result, nil
	case AggregateP50, AggregateP90, AggregateP95, AggregateP99:
		copyValues := append([]float64(nil), values...)
		sort.Float64s(copyValues)
		percentile := map[string]float64{AggregateP50: 0.50, AggregateP90: 0.90, AggregateP95: 0.95, AggregateP99: 0.99}[aggregate]
		index := int(math.Ceil(percentile*float64(len(copyValues)))) - 1
		if index < 0 {
			index = 0
		}
		return copyValues[index], nil
	default:
		return 0, fmt.Errorf("unsupported threshold aggregate %q", aggregate)
	}
}
