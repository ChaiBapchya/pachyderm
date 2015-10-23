package server

import (
	"github.com/dancannon/gorethink"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/satori/go.uuid"
	"go.pachyderm.com/pachyderm/src/pps"
	"go.pachyderm.com/pachyderm/src/pps/persist"
	"go.pedge.io/google-protobuf"
	"go.pedge.io/pkg/time"
	"go.pedge.io/proto/time"
	"golang.org/x/net/context"
)

const (
	jobInfosTable      Table = "job_infos"
	jobStatusesTable   Table = "job_statuses"
	jobOutputsTable    Table = "job_outputs"
	jobLogsTable       Table = "job_logs"
	pipelineInfosTable Table = "pipeline_infos"

	idPrimaryKey           PrimaryKey = "id"
	jobIDPrimaryKey        PrimaryKey = "job_id"
	pipelineNamePrimaryKey PrimaryKey = "pipeline_name"

	pipelineNameIndex Index = "pipeline_name"
	jobIDIndex        Index = "job_id"
	typeIndex         Index = "type"
)

type Table string
type PrimaryKey string
type Index string

var (
	marshaller = &jsonpb.Marshaler{}

	tables = []Table{
		jobInfosTable,
		jobStatusesTable,
		jobOutputsTable,
		jobLogsTable,
		pipelineInfosTable,
	}

	tableToTableCreateOpts = map[Table][]gorethink.TableCreateOpts{
		jobInfosTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: jobIDPrimaryKey,
			},
		},
		jobStatusesTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: idPrimaryKey,
			},
		},
		jobOutputsTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: jobIDPrimaryKey,
			},
		},
		jobLogsTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: idPrimaryKey,
			},
		},
		pipelineInfosTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: pipelineNamePrimaryKey,
			},
		},
	}

	tableToIndexes = map[Table][]Index{
		jobInfosTable: []Index{
			pipelineNameIndex,
		},
		jobStatusesTable: []Index{
			jobIDIndex,
			typeIndex,
		},
		jobLogsTable: []Index{
			jobIDIndex,
		},
		pipelineInfosTable: []Index{},
	}
)

// InitDBs prepares a RethinkDB instance to be used by the rethink server.
// Rethink servers will error if they are pointed at databases that haven't had InitDBs run on them.
// InitDBs should only be run once per instance of RethinkDB, it will error if it is called a second time.
func InitDBs(address string, databaseName string) error {
	session, err := gorethink.Connect(gorethink.ConnectOpts{Address: address})
	if err != nil {
		return err
	}
	if _, err := gorethink.DBCreate(databaseName).RunWrite(session); err != nil {
		return err
	}
	for _, table := range tables {
		tableCreateOpts, ok := tableToTableCreateOpts[table]
		if ok {
			if _, err := gorethink.DB(databaseName).TableCreate(table, tableCreateOpts...).RunWrite(session); err != nil {
				return err
			}
		} else {
			if _, err := gorethink.DB(databaseName).TableCreate(table).RunWrite(session); err != nil {
				return err
			}
		}
	}
	for table, indexes := range tableToIndexes {
		for _, index := range indexes {
			if _, err := gorethink.DB(databaseName).Table(table).IndexCreate(index).RunWrite(session); err != nil {
				return err
			}
		}
	}
	return nil
}

type rethinkAPIServer struct {
	session      *gorethink.Session
	databaseName string
	timer        pkgtime.Timer
}

func newRethinkAPIServer(address string, databaseName string) (*rethinkAPIServer, error) {
	session, err := gorethink.Connect(gorethink.ConnectOpts{Address: address})
	if err != nil {
		return nil, err
	}
	return &rethinkAPIServer{
		session,
		databaseName,
		pkgtime.NewSystemTimer(),
	}, nil
}

func (a *rethinkAPIServer) Close() error {
	return a.session.Close()
}

// job_id cannot be set
// timestamp cannot be set
func (a *rethinkAPIServer) CreateJobInfo(ctx context.Context, request *persist.JobInfo) (*persist.JobInfo, error) {
	if request.JobId != "" {
		return nil, ErrIDSet
	}
	if request.CreatedAt != nil {
		return nil, ErrTimestampSet
	}
	request.JobId = newID()
	request.CreatedAt = a.now()
	if err := a.insertMessage(jobInfosTable, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (a *rethinkAPIServer) GetJobInfo(ctx context.Context, request *pps.Job) (*persist.JobInfo, error) {
	jobInfo := &persist.JobInfo{}
	if err := a.getMessageByPrimaryKey(jobInfosTable, request.Id, jobInfo); err != nil {
		return nil, err
	}
	return jobInfo, nil
}

func (a *rethinkAPIServer) GetJobInfosByPipeline(ctx context.Context, request *pps.Pipeline) (*persist.JobInfos, error) {
	jobInfoObjs, err := a.getMessagesByIndex(
		jobInfosTable,
		pipelineNameIndex,
		request.Name,
		func() proto.Message { return &persist.JobInfo{} },
		func(term gorethink.Term) gorethink.Term {
			return term.OrderBy(gorethink.Desc("created_at"))
		},
	)
	if err != nil {
		return nil, err
	}
	jobInfos := make([]*persist.JobInfo, len(jobInfoObjs))
	for i, jobInfoObj := range jobInfoObjs {
		jobInfos[i] = jobInfoObj.(*persist.JobInfo)
	}
	return &persist.JobInfos{
		JobInfo: jobInfos,
	}, nil
}

// id cannot be set
// timestamp cannot be set
func (a *rethinkAPIServer) CreateJobStatus(ctx context.Context, request *persist.JobStatus) (*persist.JobStatus, error) {
	if request.Id != "" {
		return nil, ErrIDSet
	}
	if request.Timestamp != nil {
		return nil, ErrTimestampSet
	}
	request.Id = newID()
	request.Timestamp = a.now()
	if err := a.insertMessage(jobStatusesTable, request); err != nil {
		return nil, err
	}
	return request, nil
}

// ordered by time, latest to earliest
func (a *rethinkAPIServer) GetJobStatuses(ctx context.Context, request *pps.Job) (*persist.JobStatuses, error) {
	jobStatusObjs, err := a.getMessagesByIndex(
		jobStatusesTable,
		jobIDIndex,
		request.Id,
		func() proto.Message { return &persist.JobStatus{} },
		func(term gorethink.Term) gorethink.Term {
			return term.OrderBy(gorethink.Desc("timestamp"))
		},
	)
	if err != nil {
		return nil, err
	}
	jobStatuses := make([]*persist.JobStatus, len(jobStatusObjs))
	for i, jobStatusObj := range jobStatusObjs {
		jobStatuses[i] = jobStatusObj.(*persist.JobStatus)
	}
	return &persist.JobStatuses{
		JobStatus: jobStatuses,
	}, nil
}

// id cannot be set
// timestamp cannot be set
func (a *rethinkAPIServer) CreateJobLog(ctx context.Context, request *persist.JobLog) (*persist.JobLog, error) {
	if request.Id != "" {
		return nil, ErrIDSet
	}
	if request.Timestamp != nil {
		return nil, ErrTimestampSet
	}
	request.Id = newID()
	request.Timestamp = a.now()
	if err := a.insertMessage(jobLogsTable, request); err != nil {
		return nil, err
	}
	return request, nil
}

// ordered by time, latest to earliest
func (a *rethinkAPIServer) GetJobLogs(ctx context.Context, request *pps.Job) (*persist.JobLogs, error) {
	jobLogObjs, err := a.getMessagesByIndex(
		jobLogsTable,
		jobIDIndex,
		request.Id,
		func() proto.Message { return &persist.JobLog{} },
		func(term gorethink.Term) gorethink.Term {
			return term.OrderBy(gorethink.Desc("timestamp"))
		},
	)
	if err != nil {
		return nil, err
	}
	jobLogs := make([]*persist.JobLog, len(jobLogObjs))
	for i, jobLogObj := range jobLogObjs {
		jobLogs[i] = jobLogObj.(*persist.JobLog)
	}
	return &persist.JobLogs{
		JobLog: jobLogs,
	}, nil
}

// timestamp cannot be set
func (a *rethinkAPIServer) CreatePipelineInfo(ctx context.Context, request *persist.PipelineInfo) (*persist.PipelineInfo, error) {
	if request.CreatedAt != nil {
		return nil, ErrTimestampSet
	}
	request.CreatedAt = a.now()
	if err := a.insertMessage(pipelineInfosTable, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (a *rethinkAPIServer) GetPipelineInfo(ctx context.Context, request *pps.Pipeline) (*persist.PipelineInfo, error) {
	pipelineInfo := &persist.PipelineInfo{}
	if err := a.getMessageByPrimaryKey(pipelineInfosTable, request.Name, pipelineInfo); err != nil {
		return nil, err
	}
	return pipelineInfo, nil
}

func (a *rethinkAPIServer) ListPipelineInfos(ctx context.Context, request *google_protobuf.Empty) (*persist.PipelineInfos, error) {
	pipelineInfoObjs, err := a.getAllMessages(
		pipelineInfosTable,
		func() proto.Message { return &persist.PipelineInfo{} },
		func(term gorethink.Term) gorethink.Term {
			return term.OrderBy(gorethink.Desc("created_at"))
		},
	)
	if err != nil {
		return nil, err
	}
	pipelineInfos := make([]*persist.PipelineInfo, len(pipelineInfoObjs))
	for i, pipelineInfoObj := range pipelineInfoObjs {
		pipelineInfos[i] = pipelineInfoObj.(*persist.PipelineInfo)
	}
	return &persist.PipelineInfos{
		PipelineInfo: pipelineInfos,
	}, nil
}

func (a *rethinkAPIServer) DeletePipelineInfo(ctx context.Context, request *pps.Pipeline) (*google_protobuf.Empty, error) {
	if err := a.deleteMessageByPrimaryKey(pipelineInfosTable, request.Name); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

func (a *rethinkAPIServer) insertMessage(table Table, message proto.Message) error {
	data, err := marshaller.MarshalToString(message)
	if err != nil {
		return err
	}
	_, err = a.getTerm(table).Insert(gorethink.JSON(data)).RunWrite(a.session)
	return err
}

func (a *rethinkAPIServer) getMessageByPrimaryKey(table Table, value interface{}, message proto.Message) error {
	cursor, err := a.getTerm(table).Get(value).ToJSON().Run(a.session)
	if err != nil {
		return err
	}
	data := ""
	if !cursor.Next(&data) {
		return cursor.Err()
	}
	if err := jsonpb.UnmarshalString(data, message); err != nil {
		return err
	}
	return nil
}

func (a *rethinkAPIServer) deleteMessageByPrimaryKey(table Table, value interface{}) error {
	_, err := a.getTerm(table).Get(value).Delete().RunWrite(a.session)
	return err
}

func (a *rethinkAPIServer) getMessagesByIndex(
	table Table,
	index Index,
	value interface{},
	messageConstructor func() proto.Message,
	modifiers ...func(gorethink.Term) gorethink.Term,
) ([]interface{}, error) {
	return a.getMultiple(
		a.getTerm(table).GetAllByIndex(index, value),
		messageConstructor,
		modifiers...,
	)
}

func (a *rethinkAPIServer) getAllMessages(
	table Table,
	messageConstructor func() proto.Message,
	modifiers ...func(gorethink.Term) gorethink.Term,
) ([]interface{}, error) {
	return a.getMultiple(
		a.getTerm(table),
		messageConstructor,
		modifiers...,
	)
}

func (a *rethinkAPIServer) getMultiple(
	term gorethink.Term,
	messageConstructor func() proto.Message,
	modifiers ...func(gorethink.Term) gorethink.Term,
) ([]interface{}, error) {
	for _, modifier := range modifiers {
		term = modifier(term)
	}
	term = term.Map(func(row gorethink.Term) interface{} {
		return row.ToJSON()
	})
	cursor, err := term.Run(a.session)
	if err != nil {
		return nil, err
	}
	return processMultipleCursor(cursor, messageConstructor)
}

func (a *rethinkAPIServer) getTerm(table Table) gorethink.Term {
	return gorethink.DB(a.databaseName).Table(table)
}

func (a *rethinkAPIServer) now() *google_protobuf.Timestamp {
	return prototime.TimeToTimestamp(a.timer.Now())
}

func processMultipleCursor(
	cursor *gorethink.Cursor,
	messageConstructor func() proto.Message,
) ([]interface{}, error) {
	var data []string
	if err := cursor.All(&data); err != nil {
		return nil, err
	}
	result := make([]interface{}, len(data))
	for i, datum := range data {
		message := messageConstructor()
		if err := jsonpb.UnmarshalString(datum, message); err != nil {
			return nil, err
		}
		result[i] = message
	}
	return result, nil
}

func rethinkToJSON(row gorethink.Term) interface{} {
	return row.ToJSON()
}

func newID() string {
	return uuid.NewV4().String()
}
