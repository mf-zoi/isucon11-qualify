package scenario

// load.go
// シナリオの内、loadフェーズの処理

import (
	"context"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/isucon/isucandar"
	"github.com/isucon/isucandar/agent"
	"github.com/isucon/isucandar/worker"
	"github.com/isucon/isucon11-qualify/bench/logger"
	"github.com/isucon/isucon11-qualify/bench/model"
	"github.com/isucon/isucon11-qualify/bench/service"
)

func (s *Scenario) Load(parent context.Context, step *isucandar.BenchmarkStep) error {
	defer s.jiaCancel()
	step.Result().Score.Reset()
	if s.NoLoad {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, s.LoadTimeout)
	defer cancel()

	logger.ContestantLogger.Printf("===> LOAD")
	logger.AdminLogger.Printf("LOAD INFO\n  Language: %s\n  Campaign: None\n", s.Language)
	defer logger.AdminLogger.Println("<=== LOAD END")

	/*
		TODO: 実際の負荷走行シナリオ
	*/

	//通常ユーザー
	s.AddNormalUser(ctx, step, 100)
	//マニアユーザー
	//s.AddManiacUser(ctx, step, 2)
	//企業ユーザー
	s.AddCompanyUser(ctx, step, 5)

	//ユーザーを増やす
	s.loadWaitGroup.Add(1)
	go func() {
		defer s.loadWaitGroup.Done()
		s.userAdder(ctx, step)
	}()

	<-ctx.Done()
	s.jiaCancel()
	logger.AdminLogger.Println("LOAD WAIT")
	s.loadWaitGroup.Wait()

	return nil
}

func (s *Scenario) userAdder(ctx context.Context, step *isucandar.BenchmarkStep) {
	defer logger.AdminLogger.Println("--- userAdder END")
	//TODO: パラメーター調整
	for {
		select {
		case <-time.After(5000 * time.Millisecond):
		case <-ctx.Done():
			return
		}

		errCount := step.Result().Errors.Count()
		timeoutCount, ok := errCount["timeout"]
		if !ok || timeoutCount == 0 {
			logger.ContestantLogger.Println("現レベルの負荷へ応答ができているため、負荷レベルを上昇させます")
			s.AddNormalUser(ctx, step, 20)
			s.AddCompanyUser(ctx, step, 1)
		} else if ok && timeoutCount > 0 {
			logger.ContestantLogger.Println("エラーが発生したため、負荷レベルは上昇しません")
			return
		}
	}
}

func (s *Scenario) loadNormalUser(ctx context.Context, step *isucandar.BenchmarkStep) {

	userTimer, userTimerCancel := context.WithDeadline(ctx, s.realTimeLoadFinishedAt.Add(-agent.DefaultRequestTimeout))
	defer userTimerCancel()
	select {
	case <-ctx.Done():
		return
	case <-userTimer.Done():
		return
	default:
	}
	// logger.AdminLogger.Println("Normal User start")
	// defer logger.AdminLogger.Println("Normal User END")

	user := s.initNormalUser(ctx, step)
	if user == nil {
		return
	}

	randEngine := rand.New(rand.NewSource(rand.Int63()))
	nextTargetIsuIndex := 0
	scenarioSuccess := false
	lastSolvedTime := make(map[string]time.Time)
	for _, isu := range user.IsuListOrderByCreatedAt {
		lastSolvedTime[isu.JIAIsuUUID] = s.virtualTimeStart
	}
	scenarioLoopStopper := time.After(1 * time.Millisecond) //ループ頻度調整
scenarioLoop:
	for {
		<-scenarioLoopStopper
		scenarioLoopStopper = time.After(50 * time.Millisecond) //TODO: 頻度調整
		select {
		case <-ctx.Done():
			return
		case <-userTimer.Done(): //TODO: GETリクエスト系も早めに終わるかは要検討
			return
		default:
		}
		if scenarioSuccess {
			step.AddScore(ScoreNormalUserLoop) //TODO: 得点条件の修正
		}
		scenarioSuccess = true

		//posterからconditionの取得
		user.GetConditionFromChan(ctx)
		select {
		case <-ctx.Done():
			return
		default:
		}

		//conditionを見るISUを選択
		//TODO: 乱数にする
		nextTargetIsuIndex += 1
		nextTargetIsuIndex %= len(user.IsuListOrderByCreatedAt)
		targetIsu := user.IsuListOrderByCreatedAt[nextTargetIsuIndex]

		//GET /
		dataExistTimestamp := GetConditionDataExistTimestamp(s, user)
		_, _, errs := browserGetHomeAction(ctx, user.Agent, dataExistTimestamp, true,
			func(res *http.Response, isuList []*service.Isu) []error {
				expected := user.IsuListOrderByCreatedAt
				if homeIsuLimit < len(expected) { //limit
					expected = expected[len(expected)-homeIsuLimit:]
				}
				return verifyIsuOrderByCreatedAt(res, expected, isuList)
			},
			func(res *http.Response, conditions []*service.GetIsuConditionResponse) []error {
				//conditionの検証
				err := verifyIsuConditions(
					res, user, "",
					&service.GetIsuConditionRequest{
						CursorEndTime:    dataExistTimestamp,
						CursorJIAIsuUUID: "z",
						ConditionLevel:   "critical,warning,info",
					},
					conditions,
				)
				if err != nil {
					return []error{err}
				}
				return []error{}
			},
		)
		for _, err := range errs {
			scenarioSuccess = false
			addErrorWithContext(ctx, step, err)
		}

		//GET /isu/{jia_isu_uuid}
		_, errs = browserGetIsuDetailAction(ctx, user.Agent, targetIsu.JIAIsuUUID, true)
		for _, err := range errs {
			scenarioSuccess = false
			addErrorWithContext(ctx, step, err)
		}

		if randEngine.Intn(2) < 1 {
			//TODO: リロード

			//定期的にconditionを見に行くシナリオ
			request := service.GetIsuConditionRequest{
				StartTime:        nil,
				CursorEndTime:    dataExistTimestamp,
				CursorJIAIsuUUID: "",
				ConditionLevel:   "info,warning,critical",
				Limit:            nil,
			}
			conditions := s.getIsuConditionWithScroll(ctx, step, user, targetIsu, request, 2)
			if conditions == nil {
				scenarioSuccess = false
				continue scenarioLoop
			}
			if len(conditions) == 0 {
				continue scenarioLoop
			}

			//conditionを確認して、椅子状態を改善
			solvedCondition, findTimestamp := findBadIsuState(conditions)
			if solvedCondition != model.IsuStateChangeNone && lastSolvedTime[targetIsu.JIAIsuUUID].Before(time.Unix(findTimestamp, 0)) {
				//graphを見る
				//TODO: これたぶん最新のconditionを見ているので検証コケる
				virtualDay := (findTimestamp / (24 * 60 * 60)) * (24 * 60 * 60)
				_ = getIsuGraphWithPaging(ctx, step, user, targetIsu, virtualDay, 10)
				for _, err := range errs {
					scenarioSuccess = false
					addErrorWithContext(ctx, step, err)
				}

				//状態改善
				lastSolvedTime[targetIsu.JIAIsuUUID] = time.Unix(findTimestamp, 0)
				//バッファがあるのでブロック率は低い読みで直列に投げる
				select {
				case <-ctx.Done():
					return
				case targetIsu.StreamsForScenario.StateChan <- solvedCondition:
				}
			}
		} else {

			virtualToday := (dataExistTimestamp / (24 * 60 * 60)) * (24 * 60 * 60)
			virtualToday -= 24 * 60 * 60
			graph := getIsuGraphWithPaging(ctx, step, user, targetIsu, virtualToday, 10)

			//悪いものを探す
			var errorEndAtUnix int64 = 0
			for _, g := range graph {
				if g.Data != nil && g.Data.Score < 100 {
					errorEndAtUnix = g.StartAt
				}
			}

			//悪いものがあれば、そのconditionを取る
			if errorEndAtUnix != 0 {
				startTime := errorEndAtUnix - 60*60
				//MEMO: 本来は必ず1時間幅だが、検証のためにdataExistTimestampで抑える
				cursorEndTime := errorEndAtUnix
				if dataExistTimestamp < cursorEndTime {
					cursorEndTime = dataExistTimestamp
				}
				request := service.GetIsuConditionRequest{
					StartTime:        &startTime,
					CursorEndTime:    cursorEndTime,
					CursorJIAIsuUUID: "",
					ConditionLevel:   "warning,critical",
					Limit:            nil,
				}
				conditions := s.getIsuConditionWithScroll(ctx, step, user, targetIsu, request, 0)
				if conditions == nil {
					continue scenarioLoop
				}

				//状態改善
				solvedCondition, findTimestamp := findBadIsuState(conditions)
				if solvedCondition != model.IsuStateChangeNone && lastSolvedTime[targetIsu.JIAIsuUUID].Before(time.Unix(findTimestamp, 0)) {
					lastSolvedTime[targetIsu.JIAIsuUUID] = time.Unix(findTimestamp, 0)
					select {
					case <-ctx.Done():
						return
					case targetIsu.StreamsForScenario.StateChan <- solvedCondition: //バッファがあるのでブロック率は低い読みで直列に投げる
					}
				}
			}
		}
	}
}

//ユーザーとISUの作成
func (s *Scenario) initNormalUser(ctx context.Context, step *isucandar.BenchmarkStep) *model.User {
	//ユーザー作成
	userAgent, err := s.NewAgent()
	if err != nil {
		logger.AdminLogger.Panicln(err)
	}
	user := s.NewUser(ctx, step, userAgent, model.UserTypeNormal)
	if user == nil {
		//logger.AdminLogger.Println("Normal User fail: NewUser")
		return nil //致命的でないエラー
	}
	func() {
		s.normalUsersMtx.Lock()
		defer s.normalUsersMtx.Unlock()
		s.normalUsers = append(s.normalUsers, user)
	}()

	//椅子作成
	const isuCountMax = 4 //ルートページに表示する最大数
	isuCount := rand.Intn(isuCountMax) + 1
	for i := 0; i < isuCount; i++ {
		isu := s.NewIsu(ctx, step, user, true, nil)
		if isu == nil {
			//deactivate
			for _, isu := range user.IsuListOrderByCreatedAt {
				go func(isu *model.Isu) { isu.StreamsForScenario.StateChan <- model.IsuStateChangeDelete }(isu)
			}
			for _, isu := range user.IsuListOrderByCreatedAt {
				res, err := deleteIsuAction(ctx, user.Agent, isu.JIAIsuUUID)
				if err != nil {
					step.AddError(err)
				} else if !isu.IsDeactivated() {
					step.AddError(errorInvalid(res, "deactivateが完了していません"))
				}
			}

			//logger.AdminLogger.Println("Normal User fail: NewIsu(initialize)")
			return nil //致命的でないエラー
		}
	}
	step.AddScore(ScoreNormalUserInitialize)
	return user
}

//GET /isu/condition/{jia_isu_uuid} をスクロール付きで取り、バリデーションする
func (s *Scenario) getIsuConditionWithScroll(
	ctx context.Context,
	step *isucandar.BenchmarkStep,
	user *model.User,
	targetIsu *model.Isu,
	request service.GetIsuConditionRequest,
	scrollCount int,
) []*service.GetIsuConditionResponse {
	//GET condition/{jia_isu_uuid} を取得してバリデーション
	_, conditions, errs := browserGetIsuConditionAction(ctx, user.Agent, targetIsu.JIAIsuUUID,
		request,
		func(res *http.Response, conditions []*service.GetIsuConditionResponse) []error {
			//conditionの検証
			err := verifyIsuConditions(res, user, targetIsu.JIAIsuUUID, &request, conditions)
			if err != nil {
				return []error{err}
			}
			return []error{}
		},
	)
	if len(errs) > 0 {
		for _, err := range errs {
			addErrorWithContext(ctx, step, err)
		}
		return nil
	}

	//続きがある場合はスクロール
	limit := conditionLimit
	if request.Limit != nil {
		limit = *request.Limit
	}
	for i := 0; i < scrollCount && len(conditions) == limit*(i+1); i++ {
		var conditionsTmp []*service.GetIsuConditionResponse
		request = service.GetIsuConditionRequest{
			StartTime:        request.StartTime,
			CursorEndTime:    conditions[len(conditions)-1].Timestamp,
			CursorJIAIsuUUID: "",
			ConditionLevel:   request.ConditionLevel,
			Limit:            request.Limit,
		}
		conditionsTmp, res, err := getIsuConditionAction(ctx, user.Agent, targetIsu.JIAIsuUUID, request)
		if err != nil {
			addErrorWithContext(ctx, step, err)
			return nil
		}
		//検証
		err = verifyIsuConditions(res, user, targetIsu.JIAIsuUUID, &request, conditionsTmp)
		if err != nil {
			addErrorWithContext(ctx, step, err)
			return nil
		}

		conditions = append(conditions, conditionsTmp...)
	}

	return conditions
}

func getIsuGraphWithPaging(
	ctx context.Context,
	step *isucandar.BenchmarkStep,
	user *model.User,
	targetIsu *model.Isu,
	virtualDay int64,
	scrollCount int,
) service.GraphResponse {
	//graphを見に行くシナリオ
	_, graph, errs := browserGetIsuGraphAction(ctx, user.Agent, targetIsu.JIAIsuUUID, virtualDay,
		func(res *http.Response, graph service.GraphResponse) []error {
			//検証前にデータ取得
			user.GetConditionFromChan(ctx)
			return []error{} //TODO: 検証
		},
	)
	for _, err := range errs {
		addErrorWithContext(ctx, step, err)
	}
	if len(errs) > 0 {
		return nil
	}

	//前日,... のグラフ
	for i := 0; i < scrollCount; i++ {
		virtualDay -= 24 * 60 * 60
		_, graphTmp, errs := browserGetIsuGraphAction(ctx, user.Agent, targetIsu.JIAIsuUUID, virtualDay,
			func(res *http.Response, graph service.GraphResponse) []error {
				return []error{} //TODO: 検証
			},
		)
		for _, err := range errs {
			addErrorWithContext(ctx, step, err)
		}
		if len(errs) > 0 {
			return nil
		}
		graph = append(graph, graphTmp...)
	}

	return graph
}

func findBadIsuState(conditions []*service.GetIsuConditionResponse) (model.IsuStateChange, int64) {
	var virtualTimestamp int64
	solvedCondition := model.IsuStateChangeNone
	for _, c := range conditions {
		//MEMO: 重かったらフォーマットが想定通りの前提で最適化する
		bad := false
		for _, cond := range strings.Split(c.Condition, ",") {
			keyValue := strings.Split(cond, "=")
			if len(keyValue) != 2 {
				continue //形式に従っていないものは無視
			}
			if keyValue[1] != "false" {
				bad = true
				if keyValue[0] == "is_dirty" {
					solvedCondition |= model.IsuStateChangeClear
				} else if keyValue[0] == "is_overweight" {
					solvedCondition |= model.IsuStateChangeDetectOverweight
				} else if keyValue[0] == "is_broken" {
					solvedCondition |= model.IsuStateChangeRepair
				}
			}
		}
		if bad && virtualTimestamp == 0 {
			virtualTimestamp = c.Timestamp
		}
	}

	return solvedCondition, virtualTimestamp
}

func (s *Scenario) loadCompanyUser(ctx context.Context, step *isucandar.BenchmarkStep) {

	userTimer, userTimerCancel := context.WithDeadline(ctx, s.realTimeLoadFinishedAt.Add(-agent.DefaultRequestTimeout))
	defer userTimerCancel()
	select {
	case <-ctx.Done():
		return
	case <-userTimer.Done():
		return
	default:
	}
	logger.AdminLogger.Println("Company User start")
	defer logger.AdminLogger.Println("Company User END")

	user, userAgents := s.initCompanyUser(ctx, step)
	if user == nil {
		return //致命的でないエラー
	}

	//椅子作成
	//const isuCountMax = 1000
	isuCount := rand.Intn(10) + 500
	newIsuOK := true
	for i := 0; i < isuCount; i++ {
		isu := s.NewIsu(ctx, step, user, true, nil)
		if isu == nil {
			newIsuOK = false
			break
		}
	}
	if !newIsuOK {
		//並列にdeactivate
		isuChan := make(chan *model.Isu, len(user.IsuListOrderByCreatedAt))
		for _, isu := range user.IsuListOrderByCreatedAt {
			go func(isu *model.Isu) { isu.StreamsForScenario.StateChan <- model.IsuStateChangeDelete }(isu)
			isuChan <- isu
		}
		close(isuChan)
		w, err := worker.NewWorker(func(ctx context.Context, index int) {
			agent := userAgents[index]
			_, errs := authAction(ctx, agent, user.UserID)
			for _, err := range errs {
				step.AddError(err)
				return
			}
			for isu := range isuChan {
				select {
				case <-ctx.Done():
					return
				default:
				}
				res, err := deleteIsuAction(ctx, agent, isu.JIAIsuUUID)
				if err != nil {
					step.AddError(err)
				} else if !isu.IsDeactivated() {
					step.AddError(errorInvalid(res, "deactivateが完了していません"))
				}
			}
		}, worker.WithLoopCount(int32(len(userAgents))))
		if err != nil {
			logger.AdminLogger.Panicln(err)
		}
		w.Process(ctx)
		//w.Wait()
		//MEMO: ctx.Done()の場合は、プロセスが終了していない可能性がある。

		logger.AdminLogger.Println("Company User fail: NewIsu(initialize)")
		return //致命的でないエラー
	}

	step.AddScore(ScoreCompanyUserInitialize)

	scenarioDoneCount := 0
	scenarioSuccess := false
	lastSolvedTime := make(map[string]time.Time)
	for _, isu := range user.IsuListOrderByCreatedAt {
		lastSolvedTime[isu.JIAIsuUUID] = s.virtualTimeStart
	}
	// const breakNum = 100                          //一度に壊れるISUの数
	// breakTime := time.Now().Add(20 * time.Second) //大量のISUの状態が悪化するタイミング
	// breakDelete := false                          //大量修理シナリオか、大量交換シナリオか
	scenarioLoopStopper := time.After(1 * time.Millisecond)
	for {
		<-scenarioLoopStopper
		scenarioLoopStopper = time.After(50 * time.Millisecond) //TODO: 頻度調整

		select {
		case <-ctx.Done():
			return
		case <-userTimer.Done(): //TODO: GET系リクエストも早めに止めるかは要検討
			return
		default:
		}

		if scenarioSuccess {
			scenarioDoneCount++
			step.AddScore(ScoreCompanyUserLoop) //TODO: 得点条件の修正

			//シナリオに成功している場合は椅子追加
			//TODO: 係数調整
			// for isuCount < (scenarioDoneCount/30)*50 && isuCount < isuCountMax {
			// 	isu := s.NewIsu(ctx, step, user, true)
			// 	if isu == nil {
			// 		logger.AdminLogger.Println("Company User fail: NewIsu")
			// 	} else {
			// 		isuCount++
			// 	}
			// 	//logger.AdminLogger.Printf("Company User Isu: %d\n", isuCount)
			// }
		}
		scenarioSuccess = true

		//posterからconditionの取得
		user.GetConditionFromChan(ctx)
		select {
		case <-ctx.Done():
			return
		default:
		}

		//GET /
		//TODO: ベンチはPUT isu/iconが来ないとして、304を常に許すようにします。
		dataExistTimestamp := GetConditionDataExistTimestamp(s, user)
		_, _, errs := browserGetHomeAction(ctx, user.Agent, dataExistTimestamp, true,
			func(res *http.Response, isuList []*service.Isu) []error {
				expected := user.IsuListOrderByCreatedAt
				if homeIsuLimit < len(expected) { //limit
					expected = expected[len(expected)-homeIsuLimit:]
				}
				return verifyIsuOrderByCreatedAt(res, expected, isuList)
			},
			func(res *http.Response, conditions []*service.GetIsuConditionResponse) []error {
				//conditionの検証
				err := verifyIsuConditions(
					res, user, "",
					&service.GetIsuConditionRequest{
						CursorEndTime:    dataExistTimestamp,
						CursorJIAIsuUUID: "z",
						ConditionLevel:   "critical,warning,info",
					},
					conditions,
				)
				if err != nil {
					return []error{err}
				}
				return []error{}
			},
		)
		for _, err := range errs {
			scenarioSuccess = false
			addErrorWithContext(ctx, step, err)
		}
		if !scenarioSuccess {
			continue
		}

		// if breakTime.Before(time.Now()) {
		// 	if breakDelete {
		// 		scenarioSuccess = s.exchangeCompanyIsu()
		// 	} else {
		// 		scenarioSuccess = s.repairCompanyIsu()
		// 	}
		// 	breakDelete = !breakDelete
		// 	breakTime = time.Now().Add(20 * time.Second)
		// } else {
		scenarioSuccess = s.checkCompanyConditionScenario(ctx, step, user, lastSolvedTime)
		//}
	}
}

func (s *Scenario) initCompanyUser(ctx context.Context, step *isucandar.BenchmarkStep) (*model.User, []*agent.Agent) {
	//ユーザー作成
	userAgents := make([]*agent.Agent, 10)
	for i := range userAgents {
		var err error
		userAgents[i], err = s.NewAgent()
		if err != nil {
			logger.AdminLogger.Panicln(err)
		}
	}
	user := s.NewUser(ctx, step, userAgents[0], model.UserTypeCompany)
	if user == nil {
		logger.AdminLogger.Println("Company User fail: NewUser")
		return nil, nil
	}
	func() {
		s.companyUsersMtx.Lock()
		defer s.companyUsersMtx.Unlock()
		s.companyUsers = append(s.companyUsers, user)
	}()

	//椅子作成
	//const isuCountMax = 1000
	isuCount := rand.Intn(10) + 500
	newIsuOK := true
	for i := 0; i < isuCount; i++ {
		isu := s.NewIsu(ctx, step, user, true, nil)
		if isu == nil {
			newIsuOK = false
			break
		}
	}
	if !newIsuOK {
		//並列にdeactivate
		isuChan := make(chan *model.Isu, len(user.IsuListOrderByCreatedAt))
		for _, isu := range user.IsuListOrderByCreatedAt {
			go func(isu *model.Isu) { isu.StreamsForScenario.StateChan <- model.IsuStateChangeDelete }(isu)
			isuChan <- isu
		}
		close(isuChan)
		w, err := worker.NewWorker(func(ctx context.Context, index int) {
			agent := userAgents[index]
			_, errs := authAction(ctx, agent, user.UserID)
			for _, err := range errs {
				step.AddError(err)
				return
			}
			for isu := range isuChan {
				select {
				case <-ctx.Done():
					return
				default:
				}
				res, err := deleteIsuAction(ctx, agent, isu.JIAIsuUUID)
				if err != nil {
					step.AddError(err)
				} else if !isu.IsDeactivated() {
					addErrorWithContext(ctx, step, errorInvalid(res, "deactivateが完了していません"))
				}
			}
		}, worker.WithLoopCount(int32(len(userAgents))))
		if err != nil {
			logger.AdminLogger.Panicln(err)
		}
		w.Process(ctx)
		//w.Wait()
		//MEMO: ctx.Done()の場合は、プロセスが終了していない可能性がある。

		logger.AdminLogger.Println("Company User fail: NewIsu(initialize)")
		return nil, nil
	}

	step.AddScore(ScoreCompanyUserInitialize)
	return user, userAgents
}

func (s *Scenario) checkCompanyConditionScenario(ctx context.Context, step *isucandar.BenchmarkStep, user *model.User, lastSolvedTime map[string]time.Time) bool {
	//定期的にconditionを見に行くシナリオ
	scenarioSuccess := true
	dataExistTimestamp := GetConditionDataExistTimestamp(s, user)
	request := service.GetIsuConditionRequest{
		StartTime:        nil,
		CursorEndTime:    dataExistTimestamp,
		CursorJIAIsuUUID: "z",
		ConditionLevel:   "warning,critical",
		Limit:            nil,
	}
	conditions, errs := browserGetConditionsAction(ctx, user.Agent,
		request,
		func(res *http.Response, conditions []*service.GetIsuConditionResponse) []error {
			//conditionの検証
			err := verifyIsuConditions(res, user, "", &request, conditions)
			if err != nil {
				return []error{err}
			}
			return []error{}
		},
	)
	for _, err := range errs {
		scenarioSuccess = false
		step.AddError(err)
	}
	if len(errs) > 0 || len(conditions) == 0 {
		return scenarioSuccess
	}

	//スクロール
	for i := 0; i < 2 && len(conditions) == 20*(i+1); i++ {
		var conditionsTmp []*service.GetIsuConditionResponse
		request = service.GetIsuConditionRequest{
			StartTime:        nil,
			CursorEndTime:    conditions[len(conditions)-1].Timestamp,
			CursorJIAIsuUUID: conditions[len(conditions)-1].JIAIsuUUID,
			ConditionLevel:   "warning,critical",
			Limit:            nil,
		}
		conditionsTmp, res, err := getConditionAction(ctx, user.Agent, request)
		if err != nil {
			scenarioSuccess = false
			addErrorWithContext(ctx, step, err)
			break
		}
		//検証
		err = verifyIsuConditions(res, user, "", &request, conditionsTmp)
		if err != nil {
			scenarioSuccess = false
			addErrorWithContext(ctx, step, err)
			break
		}

		conditions = append(conditions, conditionsTmp...)
	}

	//conditionを確認して、椅子状態を改善
	solvedConditions, findTimestamps := findBadIsuStateWithID(conditions)
	for targetIsuID, timestamp := range findTimestamps {
		if solvedConditions[targetIsuID] != model.IsuStateChangeNone && lastSolvedTime[targetIsuID].Before(time.Unix(timestamp, 0)) {
			//graphを見る
			virtualDay := (timestamp / (24 * 60 * 60)) * (24 * 60 * 60)
			_, _, errs := browserGetIsuGraphAction(ctx, user.Agent, targetIsuID, virtualDay,
				func(res *http.Response, getGraphResp service.GraphResponse) []error {
					// graph の検証
					err := verifyGraph(res, user, targetIsuID, &service.GetGraphRequest{Date: virtualDay}, getGraphResp)
					if err != nil {
						return []error{err}
					}
					return []error{}
				},
			)
			for _, err := range errs {
				scenarioSuccess = false
				step.AddError(err)
			}

			//状態改善
			lastSolvedTime[targetIsuID] = time.Unix(timestamp, 0)
			select {
			case <-ctx.Done():
				return false
			case user.IsuListByID[targetIsuID].StreamsForScenario.StateChan <- solvedConditions[targetIsuID]: //バッファがあるのでブロック率は低い読みで直列に投げる
			}
		}
	}

	return scenarioSuccess
}

func findBadIsuStateWithID(conditions []*service.GetIsuConditionResponse) (map[string]model.IsuStateChange, map[string]int64) {
	virtualTimestamp := make(map[string]int64)
	solvedCondition := make(map[string]model.IsuStateChange)
	for _, c := range conditions {
		//MEMO: 重かったらフォーマットが想定通りの前提で最適化する
		bad := false
		if _, ok := solvedCondition[c.JIAIsuUUID]; !ok {
			solvedCondition[c.JIAIsuUUID] = model.IsuStateChangeNone
		}
		for _, cond := range strings.Split(c.Condition, ",") {
			keyValue := strings.Split(cond, "=")
			if len(keyValue) != 2 {
				continue //形式に従っていないものは無視
			}
			if keyValue[1] != "false" {
				bad = true
				if keyValue[0] == "is_dirty" {
					solvedCondition[c.JIAIsuUUID] |= model.IsuStateChangeClear
				} else if keyValue[0] == "is_overweight" {
					solvedCondition[c.JIAIsuUUID] |= model.IsuStateChangeDetectOverweight
				} else if keyValue[0] == "is_broken" {
					solvedCondition[c.JIAIsuUUID] |= model.IsuStateChangeRepair
				}
			}
		}
		if _, ok := virtualTimestamp[c.JIAIsuUUID]; bad && !ok {
			virtualTimestamp[c.JIAIsuUUID] = c.Timestamp
		}
	}

	return solvedCondition, virtualTimestamp
}
