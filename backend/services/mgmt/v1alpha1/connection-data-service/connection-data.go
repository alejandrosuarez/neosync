package v1alpha1_connectiondataservice

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/Azure/azure-sdk-for-go/sdk/ai/azopenai"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gofrs/uuid"
	pg_queries "github.com/nucleuscloud/neosync/backend/gen/go/db/dbschemas/postgresql"
	mgmtv1alpha1 "github.com/nucleuscloud/neosync/backend/gen/go/protos/mgmt/v1alpha1"
	logger_interceptor "github.com/nucleuscloud/neosync/backend/internal/connect/interceptors/logger"
	nucleuserrors "github.com/nucleuscloud/neosync/backend/internal/errors"
	"github.com/nucleuscloud/neosync/backend/internal/nucleusdb"
	sql_manager "github.com/nucleuscloud/neosync/backend/pkg/sqlmanager"
	"google.golang.org/protobuf/types/known/structpb"
)

type DatabaseSchema struct {
	TableSchema string `db:"table_schema,omitempty"`
	TableName   string `db:"table_name,omitempty"`
	ColumnName  string `db:"column_name,omitempty"`
	DataType    string `db:"data_type,omitempty"`
}

type DateScanner struct {
	val *time.Time
}

func (ds *DateScanner) Scan(input any) error {
	if input == nil {
		return nil
	}

	switch input := input.(type) {
	case time.Time:
		*ds.val = input
		return nil
	default:
		return fmt.Errorf("unable to scan type %T into DateScanner", input)
	}
}

func (s *Service) GetConnectionDataStream(
	ctx context.Context,
	req *connect.Request[mgmtv1alpha1.GetConnectionDataStreamRequest],
	stream *connect.ServerStream[mgmtv1alpha1.GetConnectionDataStreamResponse],
) error {
	logger := logger_interceptor.GetLoggerFromContextOrDefault(ctx)
	logger = logger.With("connectionId", req.Msg.ConnectionId)
	connResp, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.ConnectionId,
	}))
	if err != nil {
		return err
	}
	connection := connResp.Msg.Connection
	_, err = s.verifyUserInAccount(ctx, connection.AccountId)
	if err != nil {
		return err
	}

	connectionTimeout := uint32(5)

	switch config := connection.ConnectionConfig.Config.(type) {
	case *mgmtv1alpha1.ConnectionConfig_MysqlConfig:
		err := s.areSchemaAndTableValid(ctx, connection, req.Msg.Schema, req.Msg.Table)
		if err != nil {
			return err
		}

		conn, err := s.sqlConnector.NewDbFromConnectionConfig(connection.ConnectionConfig, &connectionTimeout, logger)
		if err != nil {
			return err
		}
		defer conn.Close()
		db, err := conn.Open()
		if err != nil {
			return err
		}

		// used to get column names
		query := fmt.Sprintf("SELECT * FROM %s LIMIT 1;", sql_manager.BuildTable(req.Msg.Schema, req.Msg.Table))
		r, err := db.QueryContext(ctx, query)
		if err != nil && !nucleusdb.IsNoRows(err) {
			return err
		}

		columnNames, err := r.Columns()
		if err != nil {
			return err
		}

		selectQuery := fmt.Sprintf("SELECT %s FROM %s;", strings.Join(columnNames, ", "), sql_manager.BuildTable(req.Msg.Schema, req.Msg.Table))
		rows, err := db.QueryContext(ctx, selectQuery)
		if err != nil && !nucleusdb.IsNoRows(err) {
			return err
		}

		for rows.Next() {
			values := make([][]byte, len(columnNames))
			valuesWrapped := make([]any, 0, len(columnNames))
			for i := range values {
				valuesWrapped = append(valuesWrapped, &values[i])
			}
			if err := rows.Scan(valuesWrapped...); err != nil {
				return err
			}
			row := map[string][]byte{}
			for i, v := range values {
				col := columnNames[i]
				row[col] = v
			}

			if err := stream.Send(&mgmtv1alpha1.GetConnectionDataStreamResponse{Row: row}); err != nil {
				return err
			}
		}

	case *mgmtv1alpha1.ConnectionConfig_PgConfig:
		err := s.areSchemaAndTableValid(ctx, connection, req.Msg.Schema, req.Msg.Table)
		if err != nil {
			return err
		}

		conn, err := s.sqlConnector.NewPgPoolFromConnectionConfig(config.PgConfig, &connectionTimeout, logger)
		if err != nil {
			return err
		}
		db, err := conn.Open(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()

		// used to get column names
		query := fmt.Sprintf("SELECT * FROM %s LIMIT 1;", sql_manager.BuildTable(req.Msg.Schema, req.Msg.Table))
		r, err := db.Query(ctx, query)
		if err != nil && !nucleusdb.IsNoRows(err) {
			return err
		}
		defer r.Close()

		columnNames := []string{}
		for _, col := range r.FieldDescriptions() {
			columnNames = append(columnNames, col.Name)
		}

		selectQuery := fmt.Sprintf("SELECT %s FROM %s;", strings.Join(columnNames, ", "), sql_manager.BuildTable(req.Msg.Schema, req.Msg.Table))
		rows, err := db.Query(ctx, selectQuery)
		if err != nil && !nucleusdb.IsNoRows(err) {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			values := make([][]byte, len(columnNames))
			valuesWrapped := make([]any, 0, len(columnNames))

			for i, col := range r.FieldDescriptions() {
				if col.DataTypeOID == 1082 { // OID for date
					var t time.Time
					ds := DateScanner{val: &t}
					valuesWrapped = append(valuesWrapped, &ds)
				} else {
					valuesWrapped = append(valuesWrapped, &values[i])
				}
			}

			if err := rows.Scan(valuesWrapped...); err != nil {
				return err
			}
			row := map[string][]byte{}
			for i, v := range values {
				col := columnNames[i]
				if r.FieldDescriptions()[i].DataTypeOID == 1082 { // OID for date
					// Convert time.Time value to []byte
					if ds, ok := valuesWrapped[i].(*DateScanner); ok && ds.val != nil {
						row[col] = []byte(ds.val.Format(time.RFC3339))
					} else {
						row[col] = v
					}
				} else if r.FieldDescriptions()[i].DataTypeOID == 2950 { // OID for UUID
					// Convert the byte slice to a uuid.UUID type
					uuidValue, err := uuid.FromBytes(v)
					if err == nil {
						row[col] = []byte(uuidValue.String())
					} else {
						row[col] = v
					}
				} else {
					row[col] = v
				}
			}

			if err := stream.Send(&mgmtv1alpha1.GetConnectionDataStreamResponse{Row: row}); err != nil {
				return err
			}
		}
		return nil

	case *mgmtv1alpha1.ConnectionConfig_AwsS3Config:
		awsS3StreamCfg := req.Msg.StreamConfig.GetAwsS3Config()
		if awsS3StreamCfg == nil {
			return nucleuserrors.NewBadRequest("jobId or jobRunId required for AWS S3 connections")
		}

		awsS3Config := config.AwsS3Config
		s3Client, err := s.awsManager.NewS3Client(ctx, awsS3Config)
		if err != nil {
			logger.Error("unable to create AWS S3 client")
			return err
		}
		logger.Info("created AWS S3 client")

		var jobRunId string
		switch id := awsS3StreamCfg.Id.(type) {
		case *mgmtv1alpha1.AwsS3StreamConfig_JobRunId:
			jobRunId = id.JobRunId
		case *mgmtv1alpha1.AwsS3StreamConfig_JobId:
			runId, err := s.getLastestJobRunFromAwsS3(ctx, logger, s3Client, id.JobId, awsS3Config.Bucket, awsS3Config.Region)
			if err != nil {
				return err
			}
			jobRunId = runId
		default:
			return nucleuserrors.NewInternalError("unsupported AWS S3 config id")
		}

		tableName := sql_manager.BuildTable(req.Msg.Schema, req.Msg.Table)
		path := fmt.Sprintf("workflows/%s/activities/%s/data", jobRunId, tableName)
		var pageToken *string
		for {
			output, err := s.awsManager.ListObjectsV2(ctx, s3Client, awsS3Config.Region, &s3.ListObjectsV2Input{
				Bucket:            aws.String(awsS3Config.Bucket),
				Prefix:            aws.String(path),
				ContinuationToken: pageToken,
			})
			if err != nil {
				return err
			}
			if output == nil {
				logger.Info(fmt.Sprintf("0 files found for path: %s", path))
				break
			}
			for _, item := range output.Contents {
				result, err := s.awsManager.GetObject(ctx, s3Client, awsS3Config.Region, &s3.GetObjectInput{
					Bucket: aws.String(awsS3Config.Bucket),
					Key:    aws.String(*item.Key),
				})
				if err != nil {
					return err
				}

				gzr, err := gzip.NewReader(result.Body)
				if err != nil {
					result.Body.Close()
					return fmt.Errorf("error creating gzip reader: %w", err)
				}

				scanner := bufio.NewScanner(gzr)
				for scanner.Scan() {
					line := scanner.Bytes()
					var data map[string]any
					err = json.Unmarshal(line, &data)
					if err != nil {
						result.Body.Close()
						gzr.Close()
						return err
					}

					rowMap := make(map[string][]byte)
					for key, value := range data {
						var byteValue []byte
						if str, ok := value.(string); ok {
							// try converting string directly to []byte
							// prevents quoted strings
							byteValue = []byte(str)
						} else {
							// if not a string use JSON encoding
							byteValue, err = json.Marshal(value)
							if err != nil {
								result.Body.Close()
								gzr.Close()
								return err
							}
							if string(byteValue) == "null" {
								byteValue = nil
							}
						}
						rowMap[key] = byteValue
					}
					if err := stream.Send(&mgmtv1alpha1.GetConnectionDataStreamResponse{Row: rowMap}); err != nil {
						result.Body.Close()
						gzr.Close()
						return err
					}
				}
				if err := scanner.Err(); err != nil {
					result.Body.Close()
					gzr.Close()
					return err
				}
				result.Body.Close()
				gzr.Close()
			}
			if *output.IsTruncated {
				pageToken = output.NextContinuationToken
				continue
			}
			break
		}

	default:
		return nucleuserrors.NewNotImplemented("this connection config is not currently supported")
	}

	return nil
}

func (s *Service) GetConnectionSchema(
	ctx context.Context,
	req *connect.Request[mgmtv1alpha1.GetConnectionSchemaRequest],
) (*connect.Response[mgmtv1alpha1.GetConnectionSchemaResponse], error) {
	logger := logger_interceptor.GetLoggerFromContextOrDefault(ctx)
	logger = logger.With("connectionId", req.Msg.ConnectionId)
	connResp, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.ConnectionId,
	}))
	if err != nil {
		return nil, err
	}
	connection := connResp.Msg.Connection
	_, err = s.verifyUserInAccount(ctx, connection.AccountId)
	if err != nil {
		return nil, err
	}

	switch config := connection.ConnectionConfig.Config.(type) {
	case *mgmtv1alpha1.ConnectionConfig_MysqlConfig, *mgmtv1alpha1.ConnectionConfig_PgConfig:
		connectionTimeout := 5
		db, err := s.sqlmanager.NewSqlDb(ctx, logger, connection, &connectionTimeout)
		if err != nil {
			return nil, err
		}
		defer db.Db.Close()

		dbschema, err := db.Db.GetDatabaseSchema(ctx)
		if err != nil {
			return nil, err
		}

		schemas := []*mgmtv1alpha1.DatabaseColumn{}
		for _, col := range dbschema {
			var defaultColumn *string
			if col.ColumnDefault != "" {
				defaultColumn = &col.ColumnDefault
			}

			schemas = append(schemas, &mgmtv1alpha1.DatabaseColumn{
				Schema:        col.TableSchema,
				Table:         col.TableName,
				Column:        col.ColumnName,
				DataType:      col.DataType,
				IsNullable:    col.IsNullable,
				ColumnDefault: defaultColumn,
				GeneratedType: col.GeneratedType,
			})
		}

		return connect.NewResponse(&mgmtv1alpha1.GetConnectionSchemaResponse{
			Schemas: schemas,
		}), nil

	case *mgmtv1alpha1.ConnectionConfig_AwsS3Config:
		awsCfg := req.Msg.SchemaConfig.GetAwsS3Config()
		if awsCfg == nil {
			return nil, nucleuserrors.NewBadRequest("jobId or jobRunId required for AWS S3 connections")
		}

		awsS3Config := config.AwsS3Config
		s3Client, err := s.awsManager.NewS3Client(ctx, config.AwsS3Config)
		if err != nil {
			return nil, err
		}
		logger.Info("created S3 AWS session")

		var jobRunId string
		switch id := awsCfg.Id.(type) {
		case *mgmtv1alpha1.AwsS3SchemaConfig_JobRunId:
			jobRunId = id.JobRunId
		case *mgmtv1alpha1.AwsS3SchemaConfig_JobId:
			runId, err := s.getLastestJobRunFromAwsS3(ctx, logger, s3Client, id.JobId, awsS3Config.Bucket, awsS3Config.Region)
			if err != nil {
				return nil, err
			}
			jobRunId = runId
		default:
			return nil, nucleuserrors.NewInternalError("unsupported AWS S3 config id")
		}

		path := fmt.Sprintf("workflows/%s/activities/", jobRunId)

		schemas := []*mgmtv1alpha1.DatabaseColumn{}
		var pageToken *string
		for {
			output, err := s.awsManager.ListObjectsV2(ctx, s3Client, awsS3Config.Region, &s3.ListObjectsV2Input{
				Bucket:            aws.String(awsS3Config.Bucket),
				Prefix:            aws.String(path),
				Delimiter:         aws.String("/"),
				ContinuationToken: pageToken,
			})
			if err != nil {
				return nil, err
			}
			if output == nil {
				break
			}
			for _, cp := range output.CommonPrefixes {
				folders := strings.Split(*cp.Prefix, "activities")
				tableFolder := strings.ReplaceAll(folders[len(folders)-1], "/", "")
				schemaTableList := strings.Split(tableFolder, ".")

				filePath := fmt.Sprintf("%s%s/data", path, sql_manager.BuildTable(schemaTableList[0], schemaTableList[1]))
				out, err := s.awsManager.ListObjectsV2(ctx, s3Client, awsS3Config.Region, &s3.ListObjectsV2Input{
					Bucket:  aws.String(awsS3Config.Bucket),
					Prefix:  aws.String(filePath),
					MaxKeys: aws.Int32(1),
				})
				if err != nil {
					return nil, err
				}
				if out == nil {
					break
				}
				item := out.Contents[0]
				result, err := s.awsManager.GetObject(ctx, s3Client, awsS3Config.Region, &s3.GetObjectInput{
					Bucket: aws.String(awsS3Config.Bucket),
					Key:    aws.String(*item.Key),
				})
				if err != nil {
					return nil, err
				}

				gzr, err := gzip.NewReader(result.Body)
				if err != nil {
					result.Body.Close()
					return nil, fmt.Errorf("error creating gzip reader: %w", err)
				}

				scanner := bufio.NewScanner(gzr)
				if scanner.Scan() {
					line := scanner.Bytes()
					var data map[string]any
					err = json.Unmarshal(line, &data)
					if err != nil {
						result.Body.Close()
						gzr.Close()
						return nil, err
					}

					for key := range data {
						schemas = append(schemas, &mgmtv1alpha1.DatabaseColumn{
							Schema: schemaTableList[0],
							Table:  schemaTableList[1],
							Column: key,
						})
					}
				}
				if err := scanner.Err(); err != nil {
					result.Body.Close()
					gzr.Close()
					return nil, err
				}
				result.Body.Close()
				gzr.Close()
			}
			if *output.IsTruncated {
				pageToken = output.NextContinuationToken
				continue
			}
			break
		}
		return connect.NewResponse(&mgmtv1alpha1.GetConnectionSchemaResponse{
			Schemas: schemas,
		}), nil

	default:
		return nil, nucleuserrors.NewNotImplemented("this connection config is not currently supported")
	}
}

func (s *Service) GetConnectionForeignConstraints(
	ctx context.Context,
	req *connect.Request[mgmtv1alpha1.GetConnectionForeignConstraintsRequest],
) (*connect.Response[mgmtv1alpha1.GetConnectionForeignConstraintsResponse], error) {
	logger := logger_interceptor.GetLoggerFromContextOrDefault(ctx)
	connection, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.ConnectionId,
	}))
	if err != nil {
		return nil, err
	}

	_, err = s.verifyUserInAccount(ctx, connection.Msg.Connection.AccountId)
	if err != nil {
		return nil, err
	}

	schemaResp, err := s.getConnectionSchema(ctx, connection.Msg.Connection, &schemaOpts{})
	if err != nil {
		return nil, err
	}

	schemaMap := map[string]struct{}{}
	for _, s := range schemaResp {
		schemaMap[s.Schema] = struct{}{}
	}
	schemas := []string{}
	for s := range schemaMap {
		schemas = append(schemas, s)
	}

	connectionTimeout := 5
	db, err := s.sqlmanager.NewSqlDb(ctx, logger, connection.Msg.GetConnection(), &connectionTimeout)
	if err != nil {
		return nil, err
	}
	defer db.Db.Close()
	foreignKeyMap, err := db.Db.GetForeignKeyConstraintsMap(ctx, schemas)
	if err != nil {
		return nil, err
	}

	tableConstraints := map[string]*mgmtv1alpha1.ForeignConstraintTables{}
	for tableName, d := range foreignKeyMap {
		tableConstraints[tableName] = &mgmtv1alpha1.ForeignConstraintTables{
			Constraints: []*mgmtv1alpha1.ForeignConstraint{},
		}
		for _, constraint := range d {
			for idx, col := range constraint.Columns {
				tableConstraints[tableName].Constraints = append(tableConstraints[tableName].Constraints, &mgmtv1alpha1.ForeignConstraint{
					Column: col, IsNullable: !constraint.NotNullable[idx], ForeignKey: &mgmtv1alpha1.ForeignKey{
						Table:  constraint.ForeignKey.Table,
						Column: constraint.ForeignKey.Columns[idx],
					},
				})
			}
		}
	}

	return connect.NewResponse(&mgmtv1alpha1.GetConnectionForeignConstraintsResponse{
		TableConstraints: tableConstraints,
	}), nil
}

func (s *Service) GetConnectionPrimaryConstraints(
	ctx context.Context,
	req *connect.Request[mgmtv1alpha1.GetConnectionPrimaryConstraintsRequest],
) (*connect.Response[mgmtv1alpha1.GetConnectionPrimaryConstraintsResponse], error) {
	logger := logger_interceptor.GetLoggerFromContextOrDefault(ctx)
	connection, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.ConnectionId,
	}))
	if err != nil {
		return nil, err
	}

	_, err = s.verifyUserInAccount(ctx, connection.Msg.Connection.AccountId)
	if err != nil {
		return nil, err
	}

	schemaResp, err := s.getConnectionSchema(ctx, connection.Msg.Connection, &schemaOpts{})
	if err != nil {
		return nil, err
	}

	schemaMap := map[string]struct{}{}
	for _, s := range schemaResp {
		schemaMap[s.Schema] = struct{}{}
	}
	schemas := []string{}
	for s := range schemaMap {
		schemas = append(schemas, s)
	}

	connectionTimeout := 5
	db, err := s.sqlmanager.NewSqlDb(ctx, logger, connection.Msg.GetConnection(), &connectionTimeout)
	if err != nil {
		return nil, err
	}
	defer db.Db.Close()

	primaryKeysMap, err := db.Db.GetPrimaryKeyConstraintsMap(ctx, schemas)
	if err != nil {
		return nil, err
	}

	tableConstraints := map[string]*mgmtv1alpha1.PrimaryConstraint{}
	for tableName, cols := range primaryKeysMap {
		tableConstraints[tableName] = &mgmtv1alpha1.PrimaryConstraint{
			Columns: cols,
		}
	}

	return connect.NewResponse(&mgmtv1alpha1.GetConnectionPrimaryConstraintsResponse{
		TableConstraints: tableConstraints,
	}), nil
}

func (s *Service) GetConnectionInitStatements(
	ctx context.Context,
	req *connect.Request[mgmtv1alpha1.GetConnectionInitStatementsRequest],
) (*connect.Response[mgmtv1alpha1.GetConnectionInitStatementsResponse], error) {
	logger := logger_interceptor.GetLoggerFromContextOrDefault(ctx)
	connection, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.ConnectionId,
	}))
	if err != nil {
		return nil, err
	}

	_, err = s.verifyUserInAccount(ctx, connection.Msg.Connection.AccountId)
	if err != nil {
		return nil, err
	}

	schemaResp, err := s.getConnectionSchema(ctx, connection.Msg.Connection, &schemaOpts{})
	if err != nil {
		return nil, err
	}

	schemaTableMap := map[string]*mgmtv1alpha1.DatabaseColumn{}
	for _, s := range schemaResp {
		schemaTableMap[sql_manager.BuildTable(s.Schema, s.Table)] = s
	}

	connectionTimeout := 5
	db, err := s.sqlmanager.NewSqlDb(ctx, logger, connection.Msg.GetConnection(), &connectionTimeout)
	if err != nil {
		return nil, err
	}
	defer db.Db.Close()

	createStmtsMap := map[string]string{}
	truncateStmtsMap := map[string]string{}
	if req.Msg.GetOptions().GetInitSchema() {
		for k, v := range schemaTableMap {
			stmt, err := db.Db.GetCreateTableStatement(ctx, v.Schema, v.Table)
			if err != nil {
				return nil, err
			}
			createStmtsMap[k] = stmt
		}
	}

	switch connection.Msg.Connection.ConnectionConfig.Config.(type) {
	case *mgmtv1alpha1.ConnectionConfig_MysqlConfig:
		if req.Msg.GetOptions().GetTruncateBeforeInsert() {
			for k, v := range schemaTableMap {
				stmt, err := sql_manager.BuildMysqlTruncateStatement(v.Schema, v.Table)
				if err != nil {
					return nil, err
				}
				truncateStmtsMap[k] = stmt
			}
		}

	case *mgmtv1alpha1.ConnectionConfig_PgConfig:
		if req.Msg.GetOptions().GetTruncateCascade() {
			for k, v := range schemaTableMap {
				stmt, err := sql_manager.BuildPgTruncateCascadeStatement(v.Schema, v.Table)
				if err != nil {
					return nil, err
				}
				truncateStmtsMap[k] = stmt
			}
		} else if req.Msg.GetOptions().GetTruncateBeforeInsert() {
			return nil, nucleuserrors.NewNotImplemented("postgres truncate unsupported. table foreig keys required to build truncate statement.")
		}

	default:
		return nil, errors.New("unsupported connection config")
	}

	return connect.NewResponse(&mgmtv1alpha1.GetConnectionInitStatementsResponse{
		TableInitStatements:     createStmtsMap,
		TableTruncateStatements: truncateStmtsMap,
	}), nil
}

type schemaOpts struct {
	JobId    *string
	JobRunId *string
}

func (s *Service) getConnectionSchema(ctx context.Context, connection *mgmtv1alpha1.Connection, opts *schemaOpts) ([]*mgmtv1alpha1.DatabaseColumn, error) {
	schemaReq := &mgmtv1alpha1.GetConnectionSchemaRequest{
		ConnectionId: connection.Id,
	}
	switch connection.ConnectionConfig.Config.(type) {
	case *mgmtv1alpha1.ConnectionConfig_PgConfig:
		schemaReq.SchemaConfig = &mgmtv1alpha1.ConnectionSchemaConfig{
			Config: &mgmtv1alpha1.ConnectionSchemaConfig_PgConfig{
				PgConfig: &mgmtv1alpha1.PostgresSchemaConfig{},
			},
		}
	case *mgmtv1alpha1.ConnectionConfig_MysqlConfig:
		schemaReq.SchemaConfig = &mgmtv1alpha1.ConnectionSchemaConfig{
			Config: &mgmtv1alpha1.ConnectionSchemaConfig_MysqlConfig{
				MysqlConfig: &mgmtv1alpha1.MysqlSchemaConfig{},
			},
		}
	case *mgmtv1alpha1.ConnectionConfig_AwsS3Config:
		var cfg *mgmtv1alpha1.AwsS3SchemaConfig
		if opts.JobRunId != nil && *opts.JobRunId != "" {
			cfg = &mgmtv1alpha1.AwsS3SchemaConfig{Id: &mgmtv1alpha1.AwsS3SchemaConfig_JobRunId{JobRunId: *opts.JobRunId}}
		} else if opts.JobId != nil && *opts.JobId != "" {
			cfg = &mgmtv1alpha1.AwsS3SchemaConfig{Id: &mgmtv1alpha1.AwsS3SchemaConfig_JobId{JobId: *opts.JobId}}
		}
		schemaReq.SchemaConfig = &mgmtv1alpha1.ConnectionSchemaConfig{
			Config: &mgmtv1alpha1.ConnectionSchemaConfig_AwsS3Config{
				AwsS3Config: cfg,
			},
		}

	default:
		return nil, nucleuserrors.NewNotImplemented("this connection config is not currently supported")
	}
	schemaResp, err := s.GetConnectionSchema(ctx, connect.NewRequest(schemaReq))
	if err != nil {
		return nil, err
	}
	return schemaResp.Msg.GetSchemas(), nil
}

func (s *Service) getConnectionTableSchema(ctx context.Context, connection *mgmtv1alpha1.Connection, schema, table string, logger *slog.Logger) ([]*mgmtv1alpha1.DatabaseColumn, error) {
	conntimeout := uint32(5)
	switch cconfig := connection.ConnectionConfig.Config.(type) {
	case *mgmtv1alpha1.ConnectionConfig_PgConfig:
		conn, err := s.sqlConnector.NewPgPoolFromConnectionConfig(cconfig.PgConfig, &conntimeout, logger)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		db, err := conn.Open(ctx)
		if err != nil {
			return nil, err
		}
		dbschema, err := s.pgquerier.GetDatabaseTableSchema(ctx, db, &pg_queries.GetDatabaseTableSchemaParams{Schema: schema, Table: table})
		if err != nil {
			return nil, err
		}
		schemas := []*mgmtv1alpha1.DatabaseColumn{}
		for _, col := range dbschema {
			schemas = append(schemas, &mgmtv1alpha1.DatabaseColumn{
				Schema:     col.SchemaName,
				Table:      col.TableName,
				Column:     col.ColumnName,
				DataType:   col.DataType,
				IsNullable: col.IsNullable,
			})
		}
		return schemas, nil
	case *mgmtv1alpha1.ConnectionConfig_MysqlConfig:
		conn, err := s.sqlConnector.NewDbFromConnectionConfig(connection.ConnectionConfig, &conntimeout, logger)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		db, err := conn.Open()
		if err != nil {
			return nil, err
		}
		dbschema, err := s.mysqlquerier.GetDatabaseSchema(ctx, db)
		if err != nil {
			return nil, err
		}
		schemas := []*mgmtv1alpha1.DatabaseColumn{}
		for _, col := range dbschema {
			if col.TableSchema != schema || col.TableName != table {
				continue
			}
			schemas = append(schemas, &mgmtv1alpha1.DatabaseColumn{
				Schema:     col.TableSchema,
				Table:      col.TableName,
				Column:     col.ColumnName,
				DataType:   col.DataType,
				IsNullable: col.IsNullable,
			})
		}
		return schemas, nil
	default:
		return nil, nucleuserrors.NewBadRequest("this connection config is not currently supported")
	}
}

// returns the first job run id for a given job that is in S3
func (s *Service) getLastestJobRunFromAwsS3(
	ctx context.Context,
	logger *slog.Logger,
	s3Client *s3.Client,
	jobId, bucket string,
	region *string,
) (string, error) {
	jobRunsResp, err := s.jobService.GetJobRecentRuns(ctx, connect.NewRequest(&mgmtv1alpha1.GetJobRecentRunsRequest{
		JobId: jobId,
	}))
	if err != nil {
		return "", err
	}
	jobRuns := jobRunsResp.Msg.GetRecentRuns()

	for i := len(jobRuns) - 1; i >= 0; i-- {
		runId := jobRuns[i].JobRunId
		path := fmt.Sprintf("workflows/%s/activities/", runId)
		output, err := s.awsManager.ListObjectsV2(ctx, s3Client, region, &s3.ListObjectsV2Input{
			Bucket:    aws.String(bucket),
			Prefix:    aws.String(path),
			Delimiter: aws.String("/"),
		})
		if err != nil {
			return "", err
		}
		if output == nil {
			continue
		}
		if *output.KeyCount > 0 {
			logger.Info(fmt.Sprintf("found latest job run: %s", runId))
			return runId, nil
		}
	}
	return "", nucleuserrors.NewInternalError(fmt.Sprintf("unable to find latest job run for job: %s", jobId))
}

func (s *Service) areSchemaAndTableValid(ctx context.Context, connection *mgmtv1alpha1.Connection, schema, table string) error {
	schemas, err := s.getConnectionSchema(ctx, connection, &schemaOpts{})
	if err != nil {
		return err
	}

	if !isValidSchema(schema, schemas) || !isValidTable(table, schemas) {
		return nucleuserrors.NewBadRequest("must provide valid schema and table")
	}
	return nil
}

func isValidTable(table string, columns []*mgmtv1alpha1.DatabaseColumn) bool {
	for _, c := range columns {
		if c.Table == table {
			return true
		}
	}
	return false
}

func isValidSchema(schema string, columns []*mgmtv1alpha1.DatabaseColumn) bool {
	for _, c := range columns {
		if c.Schema == schema {
			return true
		}
	}
	return false
}

func (s *Service) GetConnectionUniqueConstraints(
	ctx context.Context,
	req *connect.Request[mgmtv1alpha1.GetConnectionUniqueConstraintsRequest],
) (*connect.Response[mgmtv1alpha1.GetConnectionUniqueConstraintsResponse], error) {
	logger := logger_interceptor.GetLoggerFromContextOrDefault(ctx)
	connection, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.ConnectionId,
	}))
	if err != nil {
		return nil, err
	}

	_, err = s.verifyUserInAccount(ctx, connection.Msg.Connection.AccountId)
	if err != nil {
		return nil, err
	}

	schemaResp, err := s.getConnectionSchema(ctx, connection.Msg.Connection, &schemaOpts{})
	if err != nil {
		return nil, err
	}

	schemaMap := map[string]struct{}{}
	for _, s := range schemaResp {
		schemaMap[s.Schema] = struct{}{}
	}
	schemas := []string{}
	for s := range schemaMap {
		schemas = append(schemas, s)
	}

	connectionTimeout := 5
	db, err := s.sqlmanager.NewSqlDb(ctx, logger, connection.Msg.GetConnection(), &connectionTimeout)
	if err != nil {
		return nil, err
	}
	defer db.Db.Close()

	ucMap, err := db.Db.GetUniqueConstraintsMap(ctx, schemas)
	if err != nil {
		return nil, err
	}

	tableConstraints := map[string]*mgmtv1alpha1.UniqueConstraint{}
	for tableName, uc := range ucMap {
		columns := []string{}
		for _, c := range uc {
			columns = append(columns, c...)
		}
		tableConstraints[tableName] = &mgmtv1alpha1.UniqueConstraint{
			// TODO: this doesn't fully represent unique constraints
			Columns: columns,
		}
	}

	return connect.NewResponse(&mgmtv1alpha1.GetConnectionUniqueConstraintsResponse{
		TableConstraints: tableConstraints,
	}), nil
}

type completionResponse struct {
	Data []map[string]any `json:"data"`
}

func (s *Service) GetAiGeneratedData(
	ctx context.Context,
	req *connect.Request[mgmtv1alpha1.GetAiGeneratedDataRequest],
) (*connect.Response[mgmtv1alpha1.GetAiGeneratedDataResponse], error) {
	logger := logger_interceptor.GetLoggerFromContextOrDefault(ctx)
	_ = logger
	aiconnectionResp, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.GetAiConnectionId(),
	}))
	if err != nil {
		return nil, err
	}
	aiconnection := aiconnectionResp.Msg.GetConnection()
	_, err = s.verifyUserInAccount(ctx, aiconnection.GetAccountId())
	if err != nil {
		return nil, err
	}

	dbconnectionResp, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.GetDataConnectionId(),
	}))
	if err != nil {
		return nil, err
	}
	dbcols, err := s.getConnectionTableSchema(ctx, dbconnectionResp.Msg.GetConnection(), req.Msg.GetTable().GetSchema(), req.Msg.GetTable().GetTable(), logger)
	if err != nil {
		return nil, err
	}

	columns := make([]string, 0, len(dbcols))
	for _, dbcol := range dbcols {
		columns = append(columns, fmt.Sprintf("%s is %s", dbcol.Column, dbcol.DataType))
	}

	openaiconfig := aiconnection.GetConnectionConfig().GetOpenaiConfig()
	if openaiconfig == nil {
		return nil, nucleuserrors.NewBadRequest("connection must be a valid openai connection")
	}

	client, err := azopenai.NewClientForOpenAI(openaiconfig.GetApiUrl(), azcore.NewKeyCredential(openaiconfig.GetApiKey()), &azopenai.ClientOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to init openai client: %w", err)
	}

	conversation := []azopenai.ChatRequestMessageClassification{
		&azopenai.ChatRequestSystemMessage{
			Content: ptr(fmt.Sprintf("You generate data in JSON format. Generate %d records in a json array located on the data key", req.Msg.GetCount())),
		},
		&azopenai.ChatRequestUserMessage{
			Content: azopenai.NewChatRequestUserMessageContent(fmt.Sprintf("%s\n%s", req.Msg.GetUserPrompt(), fmt.Sprintf("Each record looks like this: %s", strings.Join(columns, ",")))),
		},
	}

	chatResp, err := client.GetChatCompletions(ctx, azopenai.ChatCompletionsOptions{
		Temperature:      ptr(float32(1.0)),
		DeploymentName:   ptr(req.Msg.GetModelName()),
		TopP:             ptr(float32(1.0)),
		FrequencyPenalty: ptr(float32(0)),
		N:                ptr(int32(1)),
		ResponseFormat:   &azopenai.ChatCompletionsJSONResponseFormat{},
		Messages:         conversation,
	}, &azopenai.GetChatCompletionsOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to get chat completions: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return nil, errors.New("received no choices back from openai")
	}
	choice := chatResp.Choices[0]

	if *choice.FinishReason == azopenai.CompletionsFinishReasonTokenLimitReached {
		return nil, errors.New("completion limit reached")
	}

	var dataResponse completionResponse
	err = json.Unmarshal([]byte(*choice.Message.Content), &dataResponse)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal openai message content into expected response: %w", err)
	}

	dtoRecords := []*structpb.Struct{}
	for _, record := range dataResponse.Data {
		dto, err := structpb.NewStruct(record)
		if err != nil {
			return nil, fmt.Errorf("unable to convert response data to dto struct: %w", err)
		}
		dtoRecords = append(dtoRecords, dto)
	}

	return connect.NewResponse(&mgmtv1alpha1.GetAiGeneratedDataResponse{Records: dtoRecords}), nil
}

func ptr[T any](val T) *T {
	return &val
}

func (s *Service) GetConnectionTableConstraints(
	ctx context.Context,
	req *connect.Request[mgmtv1alpha1.GetConnectionTableConstraintsRequest],
) (*connect.Response[mgmtv1alpha1.GetConnectionTableConstraintsResponse], error) {
	logger := logger_interceptor.GetLoggerFromContextOrDefault(ctx)
	connection, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.ConnectionId,
	}))
	if err != nil {
		return nil, err
	}

	_, err = s.verifyUserInAccount(ctx, connection.Msg.Connection.AccountId)
	if err != nil {
		return nil, err
	}

	schemaResp, err := s.getConnectionSchema(ctx, connection.Msg.Connection, &schemaOpts{})
	if err != nil {
		return nil, err
	}

	schemaMap := map[string]struct{}{}
	for _, s := range schemaResp {
		schemaMap[s.Schema] = struct{}{}
	}
	schemas := []string{}
	for s := range schemaMap {
		schemas = append(schemas, s)
	}

	connectionTimeout := 5
	db, err := s.sqlmanager.NewSqlDb(ctx, logger, connection.Msg.GetConnection(), &connectionTimeout)
	if err != nil {
		return nil, err
	}
	defer db.Db.Close()
	tableConstraints, err := db.Db.GetTableConstraintsBySchema(ctx, schemas)
	if err != nil {
		return nil, err
	}

	fkConstraintsMap := map[string]*mgmtv1alpha1.ForeignConstraintTables{}
	for tableName, d := range tableConstraints.ForeignKeyConstraints {
		fkConstraintsMap[tableName] = &mgmtv1alpha1.ForeignConstraintTables{
			Constraints: []*mgmtv1alpha1.ForeignConstraint{},
		}
		for _, constraint := range d {
			fkConstraintsMap[tableName].Constraints = append(fkConstraintsMap[tableName].Constraints, &mgmtv1alpha1.ForeignConstraint{
				Columns: constraint.Columns, NotNullable: constraint.NotNullable, ForeignKey: &mgmtv1alpha1.ForeignKey{
					Table:   constraint.ForeignKey.Table,
					Columns: constraint.ForeignKey.Columns,
				},
			})
		}
	}

	pkConstraintsMap := map[string]*mgmtv1alpha1.PrimaryConstraint{}
	for table, pks := range tableConstraints.PrimaryKeyConstraints {
		pkConstraintsMap[table] = &mgmtv1alpha1.PrimaryConstraint{
			Columns: pks,
		}
	}

	uniqueConstraintsMap := map[string]*mgmtv1alpha1.UniqueConstraints{}
	for table, uniqueConstraints := range tableConstraints.UniqueConstraints {
		uniqueConstraintsMap[table] = &mgmtv1alpha1.UniqueConstraints{
			Constraints: []*mgmtv1alpha1.UniqueConstraint{},
		}
		for _, uc := range uniqueConstraints {
			uniqueConstraintsMap[table].Constraints = append(uniqueConstraintsMap[table].Constraints, &mgmtv1alpha1.UniqueConstraint{
				Columns: uc,
			})
		}
	}

	return connect.NewResponse(&mgmtv1alpha1.GetConnectionTableConstraintsResponse{
		ForeignKeyConstraints: fkConstraintsMap,
		PrimaryKeyConstraints: pkConstraintsMap,
		UniqueConstraints:     uniqueConstraintsMap,
	}), nil
}

func (s *Service) GetTableRowCount(
	ctx context.Context,
	req *connect.Request[mgmtv1alpha1.GetTableRowCountRequest],
) (*connect.Response[mgmtv1alpha1.GetTableRowCountResponse], error) {
	logger := logger_interceptor.GetLoggerFromContextOrDefault(ctx)
	connection, err := s.connectionService.GetConnection(ctx, connect.NewRequest(&mgmtv1alpha1.GetConnectionRequest{
		Id: req.Msg.ConnectionId,
	}))
	if err != nil {
		return nil, err
	}

	_, err = s.verifyUserInAccount(ctx, connection.Msg.Connection.AccountId)
	if err != nil {
		return nil, err
	}

	connectionTimeout := 5
	db, err := s.sqlmanager.NewSqlDb(ctx, logger, connection.Msg.GetConnection(), &connectionTimeout)
	if err != nil {
		return nil, err
	}
	defer db.Db.Close()

	count, err := db.Db.GetTableRowCount(ctx, req.Msg.Schema, req.Msg.Table, req.Msg.WhereClause)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&mgmtv1alpha1.GetTableRowCountResponse{
		Count: count,
	}), nil
}
