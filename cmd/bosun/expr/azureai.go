package expr

import (
	"context"
	"fmt"
	"strings"
	"time"

	"bosun.org/cmd/bosun/expr/parse"
	"bosun.org/opentsdb"
	ainsights "github.com/Azure/azure-sdk-for-go/services/appinsights/v1/insights"
)

func AzureAIQuery(prefix string, e *State, metric, segmentCSV string, apps AzureApplicationInsightsApps, agtype, interval, sdur, edur string) (r *Results, err error) {
	r = new(Results)
	if apps.Prefix != prefix {
		return r, fmt.Errorf(`mismatched Azure clients: attempting to use resources from client "%v" on a query with client "%v"`, apps.Prefix, prefix)
	}
	cc, clientFound := e.Backends.AzureMonitor[prefix]
	if !clientFound {
		return r, fmt.Errorf(`azure client with name "%v" not defined`, prefix)
	}
	c := cc.AIMetricsClient
	// Parse Relative Time to absolute time
	sd, err := opentsdb.ParseDuration(sdur)
	if err != nil {
		return
	}
	var ed opentsdb.Duration
	if edur != "" {
		ed, err = opentsdb.ParseDuration(edur)
		if err != nil {
			return
		}
	}
	st := e.now.Add(time.Duration(-sd)).Format(azTimeFmt)
	en := e.now.Add(time.Duration(-ed)).Format(azTimeFmt)
	var tg string
	if interval != "" {
		tg = *azureIntervalToTimegrain(interval)
	} else {
		tg = "PT1M"
	}
	segments := []ainsights.MetricsSegment{}
	for _, s := range strings.Split(segmentCSV, ",") {
		segments = append(segments, ainsights.MetricsSegment(s))
	}
	hasSegments := segments[0] != ""
	agg := []ainsights.MetricsAggregation{ainsights.MetricsAggregation(agtype)}

	seriesMap := make(map[string]Series)

	for _, app := range apps.Applications[0:2] {
		appName, err := opentsdb.Clean(app.ApplicationName)
		if err != nil {
			return r, err
		}
		cacheKey := strings.Join([]string{prefix, app.AppId, metric, fmt.Sprintf("%s/%s", st, en), tg, agtype, segmentCSV}, ":")
		getFn := func() (interface{}, error) {
			req, err := c.GetPreparer(context.Background(), app.AppId, ainsights.MetricID(metric), fmt.Sprintf("%s/%s", st, en), &tg, agg, segments, nil, "", "")
			if err != nil {
				return nil, err
			}
			var resp ainsights.MetricsResult
			e.Timer.StepCustomTiming("azureai", "query", req.URL.String(), func() {
				hr, sendErr := c.GetSender(req)
				if sendErr == nil {
					resp, err = c.GetResponder(hr)
				} else {
					err = sendErr
				}
			})
			return resp, err
		}
		if err != nil {
			return r, err
		}
		val, err, hit := e.Cache.Get(cacheKey, getFn)
		if err != nil {
			return r, err
		}
		collectCacheHit(e.Cache, "azureai_ts", hit)
		res := val.(ainsights.MetricsResult)
		if hasSegments {
			for _, seg := range *res.Value.Segments {
				basetags := opentsdb.TagSet{
					"app": appName,
				}
				next := &seg
				if len(segments) > 1 {
					next = &(*next.Segments)[0]
				}
				for i := 0; i < len(segments)-1; i++ {
					basetags[string(segments[i])] = next.AdditionalProperties[string(segments[i])].(string)
					if i != len(segments)-2 {
						next = &(*next.Segments)[0]
					}
				}
				for _, innerSeg := range *next.Segments {
					met, ok := innerSeg.AdditionalProperties[metric]
					if !ok {
						return r, fmt.Errorf("expected additional properties not found on inner segment while handling azure query")
					}
					metMap, ok := met.(map[string]interface{})
					if !ok {
						return r, fmt.Errorf("unexpected type for additional properties not found on inner segment while handling azure query")
					}
					metVal, ok := metMap[agtype]
					if !ok {
						return r, fmt.Errorf("expected aggregation value for aggregation %v not found on inner segment while handling azure query", agtype)
					}
					tags := opentsdb.TagSet{}
					if len(segments) > 0 {
						key := string(segments[len(segments)-1])
						tags[key] = innerSeg.AdditionalProperties[key].(string)
					}
					tags = tags.Merge(basetags)
					err := tags.Clean()
					if err != nil {
						return r, err
					}
					if _, ok := seriesMap[tags.Tags()]; !ok {
						seriesMap[tags.Tags()] = make(Series)
					}
					if v, ok := metVal.(float64); ok && seg.Start != nil {
						seriesMap[tags.Tags()][seg.Start.Time] = v
					}
				}
			}
		} else {
			for _, seg := range *res.Value.Segments {
				met, ok := seg.AdditionalProperties[metric]
				if !ok {
					return r, fmt.Errorf("expected additional properties not found on inner segment while handling azure query")
				}
				metMap, ok := met.(map[string]interface{})
				if !ok {
					return r, fmt.Errorf("unexpected type for additional properties not found on inner segment while handling azure query")
				}
				metVal, ok := metMap[agtype]
				if !ok {
					return r, fmt.Errorf("expected aggregation value for aggregation %v not found on inner segment while handling azure query", agtype)
				}
				tags := opentsdb.TagSet{"app": appName}
				err := tags.Clean()
				if err != nil {
					return r, err
				}
				if _, ok := seriesMap[tags.Tags()]; !ok {
					seriesMap[tags.Tags()] = make(Series)
				}
				if v, ok := metVal.(float64); ok && seg.Start != nil {
					seriesMap[tags.Tags()][seg.Start.Time] = v
				}
			}
		}
	}
	for k, series := range seriesMap {
		tags, err := opentsdb.ParseTags(k)
		if err != nil {
			return r, err
		}
		r.Results = append(r.Results, &Result{
			Value: series,
			Group: tags,
		})
	}
	return r, nil
}

type AzureApplicationInsightsApp struct {
	ApplicationName string
	AppId           string
}
type AzureApplicationInsightsApps struct {
	Applications []AzureApplicationInsightsApp
	Prefix       string
}

func AzureAIListApps(prefix string, e *State) (r *Results, err error) {
	r = new(Results)
	// Verify prefix is a defined resource and fetch the collection of clients
	key := fmt.Sprintf("AzureAIAppCache:%s:%s", prefix, time.Now().Truncate(time.Minute*1)) // https://github.com/golang/groupcache/issues/92

	getFn := func() (interface{}, error) {
		cc, clientFound := e.Backends.AzureMonitor[prefix]
		if !clientFound {
			return r, fmt.Errorf(`azure client with name "%v" not defined`, prefix)
		}
		c := cc.AIComponentsClient
		applist := AzureApplicationInsightsApps{Prefix: prefix}
		for rList, err := c.ListComplete(context.Background()); rList.NotDone(); err = rList.Next() {
			if err != nil {
				return r, err
			}
			comp := rList.Value()
			if comp.ID != nil && comp.ApplicationInsightsComponentProperties != nil && comp.ApplicationInsightsComponentProperties.AppID != nil {
				applist.Applications = append(applist.Applications, AzureApplicationInsightsApp{
					ApplicationName: *comp.Name,
					AppId:           *comp.ApplicationInsightsComponentProperties.AppID,
				})
			}
		}
		r.Results = append(r.Results, &Result{Value: applist})
		return r, nil
	}
	val, err, hit := e.Cache.Get(key, getFn)
	collectCacheHit(e.Cache, "azure_aiapplist", hit)
	if err != nil {
		return r, err
	}
	return val.(*Results), nil
}

// azAITags is the tag function for the "az" expression function
func azAITags(args []parse.Node) (parse.Tags, error) {
	tags := parse.Tags{"app": struct{}{}}
	csvTags := strings.Split(args[1].(*parse.StringNode).Text, ",")
	if len(csvTags) == 1 && csvTags[0] == "" {
		return tags, nil
	}
	for _, k := range csvTags {
		tags[k] = struct{}{}
	}
	return tags, nil
}
