package process

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ccfos/nightingale/v6/alert/astats"
	"github.com/ccfos/nightingale/v6/alert/common"
	"github.com/ccfos/nightingale/v6/alert/dispatch"
	"github.com/ccfos/nightingale/v6/alert/mute"
	"github.com/ccfos/nightingale/v6/alert/pipeline/processor/relabel"
	"github.com/ccfos/nightingale/v6/alert/queue"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/tplx"

	"github.com/robfig/cron/v3"
	"github.com/toolkits/pkg/logger"
	"github.com/toolkits/pkg/str"
)

type EventMuteHookFunc func(event *models.AlertCurEvent) bool

type ExternalProcessorsType struct {
	ExternalLock sync.RWMutex
	Processors   map[string]*Processor
}

var ExternalProcessors ExternalProcessorsType

func NewExternalProcessors() *ExternalProcessorsType {
	return &ExternalProcessorsType{
		Processors: make(map[string]*Processor),
	}
}

func (e *ExternalProcessorsType) GetExternalAlertRule(datasourceId, id int64) (*Processor, bool) {
	e.ExternalLock.RLock()
	defer e.ExternalLock.RUnlock()
	processor, has := e.Processors[common.RuleKey(datasourceId, id)]
	return processor, has
}

type HandleEventFunc func(event *models.AlertCurEvent)

type Processor struct {
	datasourceId int64
	EngineName   string

	rule                 *models.AlertRule
	fires                *AlertCurEventMap
	pendings             *AlertCurEventMap
	pendingsUseByRecover *AlertCurEventMap
	inhibit              bool

	tagsMap   map[string]string
	tagsArr   []string
	groupName string

	alertRuleCache          *memsto.AlertRuleCacheType
	TargetCache             *memsto.TargetCacheType
	TargetsOfAlertRuleCache *memsto.TargetsOfAlertRuleCacheType
	BusiGroupCache          *memsto.BusiGroupCacheType
	alertMuteCache          *memsto.AlertMuteCacheType
	datasourceCache         *memsto.DatasourceCacheType

	ctx   *ctx.Context
	Stats *astats.Stats

	HandleFireEventHook    HandleEventFunc
	HandleRecoverEventHook HandleEventFunc
	EventMuteHook          EventMuteHookFunc

	ScheduleEntry    cron.Entry
	PromEvalInterval int
}

func (p *Processor) Key() string {
	return common.RuleKey(p.datasourceId, p.rule.Id)
}

func (p *Processor) DatasourceId() int64 {
	return p.datasourceId
}

func (p *Processor) Hash() string {
	return str.MD5(fmt.Sprintf("%d_%s_%s_%d",
		p.rule.Id,
		p.rule.CronPattern,
		p.rule.RuleConfig,
		p.datasourceId,
	))
}

func NewProcessor(engineName string, rule *models.AlertRule, datasourceId int64, alertRuleCache *memsto.AlertRuleCacheType,
	targetCache *memsto.TargetCacheType, targetsOfAlertRuleCache *memsto.TargetsOfAlertRuleCacheType,
	busiGroupCache *memsto.BusiGroupCacheType, alertMuteCache *memsto.AlertMuteCacheType, datasourceCache *memsto.DatasourceCacheType, ctx *ctx.Context,
	stats *astats.Stats) *Processor {

	p := &Processor{
		EngineName:   engineName,
		datasourceId: datasourceId,
		rule:         rule,

		TargetCache:             targetCache,
		TargetsOfAlertRuleCache: targetsOfAlertRuleCache,
		BusiGroupCache:          busiGroupCache,
		alertMuteCache:          alertMuteCache,
		alertRuleCache:          alertRuleCache,
		datasourceCache:         datasourceCache,

		ctx:   ctx,
		Stats: stats,

		HandleFireEventHook:    func(event *models.AlertCurEvent) {},
		HandleRecoverEventHook: func(event *models.AlertCurEvent) {},
		EventMuteHook:          func(event *models.AlertCurEvent) bool { return false },
	}

	p.mayHandleGroup()
	return p
}

func (p *Processor) Handle(anomalyPoints []models.AnomalyPoint, from string, inhibit bool) {
	// 有可能rule的一些配置已经发生变化，比如告警接收人、callbacks等
	// 这些信息的修改是不会引起worker restart的，但是确实会影响告警处理逻辑
	// 所以，这里直接从memsto.AlertRuleCache中获取并覆盖
	p.inhibit = inhibit
	cachedRule := p.alertRuleCache.Get(p.rule.Id)
	if cachedRule == nil {
		logger.Errorf("rule not found %+v", anomalyPoints)
		p.Stats.CounterRuleEvalErrorTotal.WithLabelValues(fmt.Sprintf("%v", p.DatasourceId()), "handle_event", p.BusiGroupCache.GetNameByBusiGroupId(p.rule.GroupId), fmt.Sprintf("%v", p.rule.Id)).Inc()
		return
	}

	// 在 rule 变化之前取到 ruleHash
	ruleHash := p.rule.Hash()

	p.rule = cachedRule
	now := time.Now().Unix()
	alertingKeys := map[string]struct{}{}

	// 根据 event 的 tag 将 events 分组，处理告警抑制的情况
	eventsMap := make(map[string][]*models.AlertCurEvent)
	for _, anomalyPoint := range anomalyPoints {
		event := p.BuildEvent(anomalyPoint, from, now, ruleHash)
		event.NotifyRuleIds = cachedRule.NotifyRuleIds
		// 如果 event 被 mute 了,本质也是 fire 的状态,这里无论如何都添加到 alertingKeys 中,防止 fire 的事件自动恢复了
		hash := event.Hash
		alertingKeys[hash] = struct{}{}
		isMuted, detail, muteId := mute.IsMuted(cachedRule, event, p.TargetCache, p.alertMuteCache)
		if isMuted {
			logger.Debugf("rule_eval:%s event:%v is muted, detail:%s", p.Key(), event, detail)
			p.Stats.CounterMuteTotal.WithLabelValues(
				fmt.Sprintf("%v", event.GroupName),
				fmt.Sprintf("%v", p.rule.Id),
				fmt.Sprintf("%v", muteId),
				fmt.Sprintf("%v", p.datasourceId),
			).Inc()
			continue
		}

		if p.EventMuteHook(event) {
			logger.Debugf("rule_eval:%s event:%v is muted by hook", p.Key(), event)
			p.Stats.CounterMuteTotal.WithLabelValues(
				fmt.Sprintf("%v", event.GroupName),
				fmt.Sprintf("%v", p.rule.Id),
				fmt.Sprintf("%v", 0),
				fmt.Sprintf("%v", p.datasourceId),
			).Inc()
			continue
		}

		tagHash := TagHash(anomalyPoint)
		eventsMap[tagHash] = append(eventsMap[tagHash], event)
	}

	for _, events := range eventsMap {
		p.handleEvent(events)
	}

	if from == "inner" {
		p.HandleRecover(alertingKeys, now, inhibit)
	}
}

func (p *Processor) BuildEvent(anomalyPoint models.AnomalyPoint, from string, now int64, ruleHash string) *models.AlertCurEvent {
	p.fillTags(anomalyPoint)

	hash := Hash(p.rule.Id, p.datasourceId, anomalyPoint)
	ds := p.datasourceCache.GetById(p.datasourceId)
	var dsName string
	if ds != nil {
		dsName = ds.Name
	}

	event := p.rule.GenerateNewEvent(p.ctx)

	bg := p.BusiGroupCache.GetByBusiGroupId(p.rule.GroupId)
	if bg != nil {
		event.GroupName = bg.Name
	}

	event.TriggerTime = anomalyPoint.Timestamp
	event.TagsMap = p.tagsMap
	event.DatasourceId = p.datasourceId
	event.Cluster = dsName
	event.Hash = hash
	event.TriggerValue = anomalyPoint.ReadableValue()
	event.TriggerValues = anomalyPoint.Values
	event.TriggerValuesJson = models.EventTriggerValues{ValuesWithUnit: anomalyPoint.ValuesUnit}
	event.TagsJSON = p.tagsArr
	event.Tags = strings.Join(p.tagsArr, ",,")
	event.IsRecovered = false
	event.Callbacks = p.rule.Callbacks
	event.CallbacksJSON = p.rule.CallbacksJSON
	event.Annotations = p.rule.Annotations
	event.RuleConfig = p.rule.RuleConfig
	event.RuleConfigJson = p.rule.RuleConfigJson
	event.Severity = anomalyPoint.Severity
	event.ExtraConfig = p.rule.ExtraConfigJSON
	event.PromQl = anomalyPoint.Query
	event.RecoverConfig = anomalyPoint.RecoverConfig
	event.RuleHash = ruleHash

	if anomalyPoint.TriggerType == models.TriggerTypeNodata {
		event.TriggerValue = "nodata"
		ruleConfig := models.RuleQuery{}
		json.Unmarshal([]byte(p.rule.RuleConfig), &ruleConfig)
		ruleConfig.TriggerType = anomalyPoint.TriggerType
		b, _ := json.Marshal(ruleConfig)
		event.RuleConfig = string(b)
	}

	if err := json.Unmarshal([]byte(p.rule.Annotations), &event.AnnotationsJSON); err != nil {
		event.AnnotationsJSON = make(map[string]string) // 解析失败时使用空 map
		logger.Warningf("unmarshal annotations json failed: %v, rule: %d", err, p.rule.Id)
	}

	if event.TriggerValues != "" && strings.Count(event.TriggerValues, "$") > 1 {
		// TriggerValues 有多个变量，将多个变量都放到 TriggerValue 中
		event.TriggerValue = event.TriggerValues
	}

	if from == "inner" {
		event.LastEvalTime = now
	} else {
		event.LastEvalTime = event.TriggerTime
	}

	// 生成事件之后，立马进程 relabel 处理
	Relabel(p.rule, event)

	// 放到 Relabel(p.rule, event) 下面，为了处理 relabel 之后，标签里才出现 ident 的情况
	p.mayHandleIdent(event)

	if event.TargetIdent != "" {
		if pt, exist := p.TargetCache.Get(event.TargetIdent); exist {
			pt.GroupNames = p.BusiGroupCache.GetNamesByBusiGroupIds(pt.GroupIds)
			event.Target = pt
		} else {
			logger.Infof("fill event target error, ident: %s doesn't exist in cache.", event.TargetIdent)
		}
	}

	return event
}

func Relabel(rule *models.AlertRule, event *models.AlertCurEvent) {
	if rule == nil {
		return
	}

	// need to keep the original label
	event.OriginalTags = event.Tags
	event.OriginalTagsJSON = event.TagsJSON

	if len(rule.EventRelabelConfig) == 0 {
		return
	}

	relabel.EventRelabel(event, rule.EventRelabelConfig)
}

func (p *Processor) HandleRecover(alertingKeys map[string]struct{}, now int64, inhibit bool) {
	for _, hash := range p.pendings.Keys() {
		if _, has := alertingKeys[hash]; has {
			continue
		}
		p.pendings.Delete(hash)
	}

	hashArr := make([]string, 0, len(alertingKeys))
	for hash, _ := range p.fires.GetAll() {
		if _, has := alertingKeys[hash]; has {
			continue
		}

		hashArr = append(hashArr, hash)
	}
	p.HandleRecoverEvent(hashArr, now, inhibit)

}

func (p *Processor) HandleRecoverEvent(hashArr []string, now int64, inhibit bool) {
	cachedRule := p.rule
	if cachedRule == nil {
		return
	}

	if !inhibit {
		for _, hash := range hashArr {
			p.RecoverSingle(false, hash, now, nil)
		}
		return
	}

	eventMap := make(map[string]models.AlertCurEvent)
	for _, hash := range hashArr {
		event, has := p.fires.Get(hash)
		if !has {
			continue
		}

		e, exists := eventMap[event.Tags]
		if !exists {
			eventMap[event.Tags] = *event
			continue
		}

		if e.Severity > event.Severity {
			// hash 对应的恢复事件的被抑制了，把之前的事件删除
			p.fires.Delete(e.Hash)
			p.pendings.Delete(e.Hash)
			models.AlertCurEventDelByHash(p.ctx, e.Hash)
			eventMap[event.Tags] = *event
		}
	}

	for _, event := range eventMap {
		p.RecoverSingle(false, event.Hash, now, nil)
	}
}

func (p *Processor) RecoverSingle(byRecover bool, hash string, now int64, value *string, values ...string) {
	cachedRule := p.rule
	if cachedRule == nil {
		return
	}

	event, has := p.fires.Get(hash)
	if !has {
		return
	}

	// 如果配置了留观时长，就不能立马恢复了
	if cachedRule.RecoverDuration > 0 {
		lastPendingEvent, has := p.pendingsUseByRecover.Get(hash)
		if !has {
			// 说明没有产生过异常点，就不需要恢复了
			logger.Debugf("rule_eval:%s event:%v do not has pending event, not recover", p.Key(), event)
			return
		}

		if now-lastPendingEvent.LastEvalTime < cachedRule.RecoverDuration {
			logger.Debugf("rule_eval:%s event:%v not recover", p.Key(), event)
			return
		}
	}

	// 如果设置了恢复条件，则不能在此处恢复，必须依靠 recoverPoint 来恢复
	if event.RecoverConfig.JudgeType != models.Origin && !byRecover {
		logger.Debugf("rule_eval:%s event:%v not recover", p.Key(), event)
		return
	}

	if value != nil {
		event.TriggerValue = *value
		if len(values) > 0 {
			event.TriggerValues = values[0]
		}
	}

	// 没查到触发阈值的vector，姑且就认为这个vector的值恢复了
	// 我确实无法分辨，是prom中有值但是未满足阈值所以没返回，还是prom中确实丢了一些点导致没有数据可以返回，尴尬
	p.fires.Delete(hash)
	p.pendings.Delete(hash)
	p.pendingsUseByRecover.Delete(hash)

	// 可能是因为调整了promql才恢复的，所以事件里边要体现最新的promql，否则用户会比较困惑
	// 当然，其实rule的各个字段都可能发生变化了，都更新一下吧
	cachedRule.UpdateEvent(event)
	event.IsRecovered = true
	event.LastEvalTime = now

	p.HandleRecoverEventHook(event)
	p.pushEventToQueue(event)
}

func (p *Processor) handleEvent(events []*models.AlertCurEvent) {
	var fireEvents []*models.AlertCurEvent
	// severity 初始为最低优先级, 一定为遇到比自己优先级高的事件
	severity := models.SeverityLowest
	for _, event := range events {
		if event == nil {
			continue
		}

		if _, has := p.pendingsUseByRecover.Get(event.Hash); has {
			p.pendingsUseByRecover.UpdateLastEvalTime(event.Hash, event.LastEvalTime)
		} else {
			p.pendingsUseByRecover.Set(event.Hash, event)
		}

		event.PromEvalInterval = p.PromEvalInterval
		if p.rule.PromForDuration == 0 {
			fireEvents = append(fireEvents, event)
			if severity > event.Severity {
				severity = event.Severity
			}
			continue
		}

		var preEvalTime int64 // 第一个 pending event 的检测时间
		preEvent, has := p.pendings.Get(event.Hash)
		if has {
			p.pendings.UpdateLastEvalTime(event.Hash, event.LastEvalTime)
			preEvalTime = preEvent.FirstEvalTime
		} else {
			event.FirstEvalTime = event.LastEvalTime
			p.pendings.Set(event.Hash, event)
			preEvalTime = event.FirstEvalTime
		}

		if event.LastEvalTime-preEvalTime+int64(event.PromEvalInterval) >= int64(p.rule.PromForDuration) {
			fireEvents = append(fireEvents, event)
			if severity > event.Severity {
				severity = event.Severity
			}
			continue
		}
	}

	p.inhibitEvent(fireEvents, severity)
}

func (p *Processor) inhibitEvent(events []*models.AlertCurEvent, highSeverity int) {
	for _, event := range events {
		if p.inhibit && event.Severity > highSeverity {
			logger.Debugf("rule_eval:%s event:%+v inhibit highSeverity:%d", p.Key(), event, highSeverity)
			continue
		}
		p.fireEvent(event)
	}
}

func (p *Processor) fireEvent(event *models.AlertCurEvent) {
	// As p.rule maybe outdated, use rule from cache
	cachedRule := p.rule
	if cachedRule == nil {
		return
	}

	message := "unknown"
	defer func() {
		logger.Infof("rule_eval:%s event-hash-%s %s", p.Key(), event.Hash, message)
	}()

	if fired, has := p.fires.Get(event.Hash); has {
		p.fires.UpdateLastEvalTime(event.Hash, event.LastEvalTime)
		event.FirstTriggerTime = fired.FirstTriggerTime
		p.HandleFireEventHook(event)

		if cachedRule.NotifyRepeatStep == 0 {
			message = "stalled, rule.notify_repeat_step is 0, no need to repeat notify"
			return
		}

		// 之前发送过告警了，这次是否要继续发送，要看是否过了通道静默时间
		if event.LastEvalTime >= fired.LastSentTime+int64(cachedRule.NotifyRepeatStep)*60 {
			if cachedRule.NotifyMaxNumber == 0 {
				// 最大可以发送次数如果是0，表示不想限制最大发送次数，一直发即可
				event.NotifyCurNumber = fired.NotifyCurNumber + 1
				message = fmt.Sprintf("fired, notify_repeat_step_matched(%d >= %d + %d * 60) notify_max_number_ignore(#%d / %d)", event.LastEvalTime, fired.LastSentTime, cachedRule.NotifyRepeatStep, event.NotifyCurNumber, cachedRule.NotifyMaxNumber)
				p.pushEventToQueue(event)
			} else {
				// 有最大发送次数的限制，就要看已经发了几次了，是否达到了最大发送次数
				if fired.NotifyCurNumber >= cachedRule.NotifyMaxNumber {
					message = fmt.Sprintf("stalled, notify_repeat_step_matched(%d >= %d + %d * 60) notify_max_number_not_matched(#%d / %d)", event.LastEvalTime, fired.LastSentTime, cachedRule.NotifyRepeatStep, fired.NotifyCurNumber, cachedRule.NotifyMaxNumber)
					return
				} else {
					event.NotifyCurNumber = fired.NotifyCurNumber + 1
					message = fmt.Sprintf("fired, notify_repeat_step_matched(%d >= %d + %d * 60) notify_max_number_matched(#%d / %d)", event.LastEvalTime, fired.LastSentTime, cachedRule.NotifyRepeatStep, event.NotifyCurNumber, cachedRule.NotifyMaxNumber)
					p.pushEventToQueue(event)
				}
			}
		} else {
			message = fmt.Sprintf("stalled, notify_repeat_step_not_matched(%d < %d + %d * 60)", event.LastEvalTime, fired.LastSentTime, cachedRule.NotifyRepeatStep)
		}
	} else {
		event.NotifyCurNumber = 1
		event.FirstTriggerTime = event.TriggerTime
		message = fmt.Sprintf("fired, first_trigger_time: %d", event.FirstTriggerTime)
		p.HandleFireEventHook(event)
		p.pushEventToQueue(event)
	}
}

func (p *Processor) pushEventToQueue(e *models.AlertCurEvent) {
	if !e.IsRecovered {
		e.LastSentTime = e.LastEvalTime
		p.fires.Set(e.Hash, e)
	}

	dispatch.LogEvent(e, "push_queue")
	if !queue.EventQueue.PushFront(e) {
		logger.Warningf("event_push_queue: queue is full, event:%+v", e)
		p.Stats.CounterRuleEvalErrorTotal.WithLabelValues(fmt.Sprintf("%v", p.DatasourceId()), "push_event_queue", p.BusiGroupCache.GetNameByBusiGroupId(p.rule.GroupId), fmt.Sprintf("%v", p.rule.Id)).Inc()
	}
}

func (p *Processor) RecoverAlertCurEventFromDb() {
	p.pendings = NewAlertCurEventMap(nil)
	p.pendingsUseByRecover = NewAlertCurEventMap(nil)

	curEvents, err := models.AlertCurEventGetByRuleIdAndDsId(p.ctx, p.rule.Id, p.datasourceId)
	if err != nil {
		logger.Errorf("recover event from db for rule:%s failed, err:%s", p.Key(), err)
		p.Stats.CounterRuleEvalErrorTotal.WithLabelValues(fmt.Sprintf("%v", p.DatasourceId()), "get_recover_event", p.BusiGroupCache.GetNameByBusiGroupId(p.rule.GroupId), fmt.Sprintf("%v", p.rule.Id)).Inc()
		p.fires = NewAlertCurEventMap(nil)
		return
	}

	fireMap := make(map[string]*models.AlertCurEvent)
	pendingsUseByRecoverMap := make(map[string]*models.AlertCurEvent)
	for _, event := range curEvents {
		alertRule := p.alertRuleCache.Get(event.RuleId)
		if alertRule == nil {
			continue
		}
		event.NotifyRuleIds = alertRule.NotifyRuleIds

		if event.Cate == models.HOST {
			target, exists := p.TargetCache.Get(event.TargetIdent)
			if exists && target.EngineName != p.EngineName && !(p.ctx.IsCenter && target.EngineName == "") {
				// 如果是 host rule，且 target 的 engineName 不是当前的 engineName 或者是中心机房 target EngineName 为空，就跳过
				continue
			}
		}

		event.DB2Mem()
		target, exists := p.TargetCache.Get(event.TargetIdent)
		if exists {
			target.GroupNames = p.BusiGroupCache.GetNamesByBusiGroupIds(target.GroupIds)
			event.Target = target
		}

		fireMap[event.Hash] = event
		e := *event
		pendingsUseByRecoverMap[event.Hash] = &e
	}

	p.fires = NewAlertCurEventMap(fireMap)

	// 修改告警规则，或者进程重启之后，需要重新加载 pendingsUseByRecover
	p.pendingsUseByRecover = NewAlertCurEventMap(pendingsUseByRecoverMap)
}

func (p *Processor) fillTags(anomalyPoint models.AnomalyPoint) {
	// handle series tags
	tagsMap := make(map[string]string)
	for label, value := range anomalyPoint.Labels {
		tagsMap[string(label)] = string(value)
	}

	var e = &models.AlertCurEvent{
		TagsMap: tagsMap,
	}

	// handle rule tags
	tags := p.rule.AppendTagsJSON
	tags = append(tags, "rulename="+p.rule.Name)
	for _, tag := range tags {
		arr := strings.SplitN(tag, "=", 2)

		var defs = []string{
			"{{$labels := .TagsMap}}",
			"{{$value := .TriggerValue}}",
		}
		tagValue := arr[1]
		text := strings.Join(append(defs, tagValue), "")
		t, err := template.New(fmt.Sprint(p.rule.Id)).Funcs(template.FuncMap(tplx.TemplateFuncMap)).Parse(text)
		if err != nil {
			tagValue = fmt.Sprintf("parse tag value failed, err:%s", err)
			tagsMap[arr[0]] = tagValue
			continue
		}

		var body bytes.Buffer
		err = t.Execute(&body, e)
		if err != nil {
			tagValue = fmt.Sprintf("parse tag value failed, err:%s", err)
			tagsMap[arr[0]] = tagValue
			continue
		}

		tagsMap[arr[0]] = body.String()
	}
	p.tagsMap = tagsMap

	// handle tagsArr
	p.tagsArr = labelMapToArr(tagsMap)
}

func (p *Processor) mayHandleIdent(event *models.AlertCurEvent) {
	// handle ident
	if ident, has := event.TagsMap["ident"]; has {
		if target, exists := p.TargetCache.Get(ident); exists {
			event.TargetIdent = target.Ident
			event.TargetNote = target.Note
		} else {
			event.TargetIdent = ident
			event.TargetNote = ""
		}
	} else {
		event.TargetIdent = ""
		event.TargetNote = ""
	}
}

func (p *Processor) mayHandleGroup() {
	// handle bg
	bg := p.BusiGroupCache.GetByBusiGroupId(p.rule.GroupId)
	if bg != nil {
		p.groupName = bg.Name
	}
}

func (p *Processor) DeleteProcessEvent(hash string) {
	p.fires.Delete(hash)
	p.pendings.Delete(hash)
	p.pendingsUseByRecover.Delete(hash)
}

func labelMapToArr(m map[string]string) []string {
	numLabels := len(m)

	labelStrings := make([]string, 0, numLabels)
	for label, value := range m {
		labelStrings = append(labelStrings, fmt.Sprintf("%s=%s", label, value))
	}

	if numLabels > 1 {
		sort.Strings(labelStrings)
	}
	return labelStrings
}

func Hash(ruleId, datasourceId int64, vector models.AnomalyPoint) string {
	return str.MD5(fmt.Sprintf("%d_%s_%d_%d_%s", ruleId, vector.Labels.String(), datasourceId, vector.Severity, vector.Query))
}

func TagHash(vector models.AnomalyPoint) string {
	return str.MD5(vector.Labels.String())
}
