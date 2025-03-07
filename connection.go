// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2012 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

type getArgByIndex func(int) driver.Value

type mysqlConn struct {
	buf              buffer
	netConn          net.Conn
	rawConn          net.Conn // underlying connection when netConn is TLS connection.
	affectedRows     uint64
	insertId         uint64
	cfg              *Config
	maxAllowedPacket int
	maxWriteSize     int
	writeTimeout     time.Duration
	flags            clientFlag
	status           statusFlag
	sequence         uint8
	parseTime        bool
	reset            bool // set when the Go SQL package calls ResetSession

	// for context support (Go 1.8+)
	watching bool
	watcher  chan<- context.Context
	closech  chan struct{}
	finished chan<- struct{}
	canceled atomicError // set non-nil if conn is canceled
	closed   atomicBool  // set when conn is closed, before closech is closed
}

// Handles parameters set in DSN after the connection is established
func (mc *mysqlConn) handleParams() (err error) {
	var cmdSet strings.Builder
	for param, val := range mc.cfg.Params {
		switch param {
		// Charset: character_set_connection, character_set_client, character_set_results
		case "charset":
			charsets := strings.Split(val, ",")
			for i := range charsets {
				// ignore errors here - a charset may not exist
				err = mc.exec("SET NAMES " + charsets[i])
				if err == nil {
					break
				}
			}
			if err != nil {
				return
			}

		// Other system vars accumulated in a single SET command
		default:
			if cmdSet.Len() == 0 {
				// Heuristic: 29 chars for each other key=value to reduce reallocations
				cmdSet.Grow(4 + len(param) + 1 + len(val) + 30*(len(mc.cfg.Params)-1))
				cmdSet.WriteString("SET ")
			} else {
				cmdSet.WriteByte(',')
			}
			cmdSet.WriteString(param)
			cmdSet.WriteByte('=')
			cmdSet.WriteString(val)
		}
	}

	if cmdSet.Len() > 0 {
		err = mc.exec(cmdSet.String())
		if err != nil {
			return
		}
	}

	return
}

func (mc *mysqlConn) markBadConn(err error) error {
	if mc == nil {
		return err
	}
	if err != errBadConnNoWrite {
		return err
	}
	return driver.ErrBadConn
}

func (mc *mysqlConn) Begin() (driver.Tx, error) {
	return mc.begin(false)
}

func (mc *mysqlConn) begin(readOnly bool) (driver.Tx, error) {
	if mc.closed.IsSet() {
		errLog.Print(ErrInvalidConn)
		return nil, driver.ErrBadConn
	}
	var q string
	if readOnly {
		q = "START TRANSACTION READ ONLY"
	} else {
		q = "START TRANSACTION"
	}
	err := mc.exec(q)
	if err == nil {
		return &mysqlTx{mc}, err
	}
	return nil, mc.markBadConn(err)
}

func (mc *mysqlConn) Close() (err error) {
	// Makes Close idempotent
	if !mc.closed.IsSet() {
		err = mc.writeCommandPacket(comQuit)
	}

	mc.cleanup()

	return
}

// Closes the network connection and unsets internal variables. Do not call this
// function after successfully authentication, call Close instead. This function
// is called before auth or on auth failure because MySQL will have already
// closed the network connection.
func (mc *mysqlConn) cleanup() {
	if !mc.closed.TrySet(true) {
		return
	}

	// Makes cleanup idempotent
	close(mc.closech)
	if mc.netConn == nil {
		return
	}
	if err := mc.netConn.Close(); err != nil {
		errLog.Print(err)
	}
}

func (mc *mysqlConn) error() error {
	if mc.closed.IsSet() {
		if err := mc.canceled.Value(); err != nil {
			return err
		}
		return ErrInvalidConn
	}
	return nil
}

func (mc *mysqlConn) beginTracing(ctx context.Context, operationName string, values ...string) (context.Context, trace.Span) {
	var spanChild trace.Span
	if mc.cfg != nil && mc.cfg.OpenTracing {
		var kvs []attribute.KeyValue
		if len(values) > 0 {
			kvs = make([]attribute.KeyValue, 0, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				if i+1 >= len(values) {
					break
				}
				kvs = append(kvs, attribute.String(values[i], values[i+1]))
			}
		}
		ctx, spanChild = otel.Tracer("").Start(ctx, operationName, trace.WithAttributes(kvs...), trace.WithSpanKind(trace.SpanKindClient))
	}
	return ctx, spanChild
}

func (mc *mysqlConn) finishTracing(spanChild trace.Span) {
	if spanChild != nil {
		spanChild.End()
	}
}

func (mc *mysqlConn) Prepare(query string) (driver.Stmt, error) {
	if mc.closed.IsSet() {
		errLog.Print(ErrInvalidConn)
		return nil, driver.ErrBadConn
	}
	// Send command
	err := mc.writeCommandPacketStr(comStmtPrepare, query)
	if err != nil {
		// STMT_PREPARE is safe to retry.  So we can return ErrBadConn here.
		errLog.Print(err)
		return nil, driver.ErrBadConn
	}

	stmt := &mysqlStmt{
		mc: mc,
	}

	// Read Result
	columnCount, err := stmt.readPrepareResultPacket()
	if err == nil {
		if stmt.paramCount > 0 {
			if err = mc.readUntilEOF(); err != nil {
				return nil, err
			}
		}

		if columnCount > 0 {
			err = mc.readUntilEOF()
		}
	}

	return stmt, err
}

func (mc *mysqlConn) interpolateParams(query string, argsLen int, getArgByIndex getArgByIndex) (string, error) {
	// Number of ? should be same to len(args)
	if strings.Count(query, "?") != argsLen {
		return "", driver.ErrSkip
	}

	buf, err := mc.buf.takeCompleteBuffer()
	if err != nil {
		// can not take the buffer. Something must be wrong with the connection
		errLog.Print(err)
		return "", ErrInvalidConn
	}
	buf = buf[:0]
	argPos := 0

	for i := 0; i < len(query); i++ {
		q := strings.IndexByte(query[i:], '?')
		if q == -1 {
			buf = append(buf, query[i:]...)
			break
		}
		buf = append(buf, query[i:i+q]...)
		i += q

		arg := getArgByIndex(argPos)
		argPos++

		if arg == nil {
			buf = append(buf, "NULL"...)
			continue
		}

		switch v := arg.(type) {
		case int64:
			buf = strconv.AppendInt(buf, v, 10)
		case uint64:
			// Handle uint64 explicitly because our custom ConvertValue emits unsigned values
			buf = strconv.AppendUint(buf, v, 10)
		case float64:
			buf = strconv.AppendFloat(buf, v, 'g', -1, 64)
		case bool:
			if v {
				buf = append(buf, '1')
			} else {
				buf = append(buf, '0')
			}
		case time.Time:
			if v.IsZero() {
				buf = append(buf, "'0000-00-00'"...)
			} else {
				buf = append(buf, '\'')
				buf, err = appendDateTime(buf, v.In(mc.cfg.Loc))
				if err != nil {
					return "", err
				}
				buf = append(buf, '\'')
			}
		case json.RawMessage:
			buf = append(buf, '\'')
			if mc.status&statusNoBackslashEscapes == 0 {
				buf = escapeBytesBackslash(buf, v)
			} else {
				buf = escapeBytesQuotes(buf, v)
			}
			buf = append(buf, '\'')
		case []byte:
			if v == nil {
				buf = append(buf, "NULL"...)
			} else {
				buf = append(buf, "_binary'"...)
				if mc.status&statusNoBackslashEscapes == 0 {
					buf = escapeBytesBackslash(buf, v)
				} else {
					buf = escapeBytesQuotes(buf, v)
				}
				buf = append(buf, '\'')
			}
		case string:
			buf = append(buf, '\'')
			if mc.status&statusNoBackslashEscapes == 0 {
				buf = escapeStringBackslash(buf, v)
			} else {
				buf = escapeStringQuotes(buf, v)
			}
			buf = append(buf, '\'')
		default:
			return "", driver.ErrSkip
		}

		if len(buf)+4 > mc.maxAllowedPacket {
			return "", driver.ErrSkip
		}
	}
	if argPos != argsLen {
		return "", driver.ErrSkip
	}
	return string(buf), nil
}

func (mc *mysqlConn) execWithArgs(query string, argsLen int, getArgByIndex getArgByIndex) (driver.Result, error) {
	if mc.closed.IsSet() {
		errLog.Print(ErrInvalidConn)
		return nil, driver.ErrBadConn
	}
	if argsLen != 0 {
		if !mc.cfg.InterpolateParams {
			return nil, driver.ErrSkip
		}
		// try to interpolate the parameters to save extra roundtrips for preparing and closing a statement
		prepared, err := mc.interpolateParams(query, argsLen, getArgByIndex)
		if err != nil {
			return nil, err
		}
		query = prepared
	}
	mc.affectedRows = 0
	mc.insertId = 0

	err := mc.exec(query)
	if err == nil {
		return &mysqlResult{
			affectedRows: int64(mc.affectedRows),
			insertId:     int64(mc.insertId),
		}, err
	}
	return nil, mc.markBadConn(err)
}

func (mc *mysqlConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	return mc.execWithArgs(query, len(args), func(i int) driver.Value { return args[i] })
}

// Internal function to execute commands
func (mc *mysqlConn) exec(query string) error {
	// Send command
	if err := mc.writeCommandPacketStr(comQuery, query); err != nil {
		return mc.markBadConn(err)
	}

	// Read Result
	resLen, err := mc.readResultSetHeaderPacket()
	if err != nil {
		return err
	}

	if resLen > 0 {
		// columns
		if err := mc.readUntilEOF(); err != nil {
			return err
		}

		// rows
		if err := mc.readUntilEOF(); err != nil {
			return err
		}
	}

	return mc.discardResults()
}

func (mc *mysqlConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	return mc.query(query, len(args), func(i int) driver.Value { return args[i] })
}

func (mc *mysqlConn) query(query string, argsLen int, getArgByIndex getArgByIndex) (*textRows, error) {
	if mc.closed.IsSet() {
		errLog.Print(ErrInvalidConn)
		return nil, driver.ErrBadConn
	}
	if argsLen != 0 {
		if !mc.cfg.InterpolateParams {
			return nil, driver.ErrSkip
		}
		// try client-side prepare to reduce roundtrip
		prepared, err := mc.interpolateParams(query, argsLen, getArgByIndex)
		if err != nil {
			return nil, err
		}
		query = prepared
	}
	// Send command
	err := mc.writeCommandPacketStr(comQuery, query)
	if err == nil {
		// Read Result
		var resLen int
		resLen, err = mc.readResultSetHeaderPacket()
		if err == nil {
			rows := new(textRows)
			rows.mc = mc

			if resLen == 0 {
				rows.rs.done = true

				switch err := rows.NextResultSet(); err {
				case nil, io.EOF:
					return rows, nil
				default:
					return nil, err
				}
			}

			// Columns
			rows.rs.columns, err = mc.readColumns(resLen)
			return rows, err
		}
	}
	return nil, mc.markBadConn(err)
}

// Gets the value of the given MySQL System Variable
// The returned byte slice is only valid until the next read
func (mc *mysqlConn) getSystemVar(name string) ([]byte, error) {
	// Send command
	if err := mc.writeCommandPacketStr(comQuery, "SELECT @@"+name); err != nil {
		return nil, err
	}

	// Read Result
	resLen, err := mc.readResultSetHeaderPacket()
	if err == nil {
		rows := new(textRows)
		rows.mc = mc
		rows.rs.columns = []mysqlField{{fieldType: fieldTypeVarChar}}

		if resLen > 0 {
			// Columns
			if err = mc.readUntilEOF(); err != nil {
				return nil, err
			}
		}

		dest := make([]driver.Value, resLen)
		if err = rows.readRow(dest); err == nil {
			return dest[0].([]byte), mc.readUntilEOF()
		}
	}
	return nil, err
}

// finish is called when the query has canceled.
func (mc *mysqlConn) cancel(err error) {
	mc.canceled.Set(err)
	mc.cleanup()
}

// finish is called when the query has succeeded.
func (mc *mysqlConn) finish() {
	if !mc.watching || mc.finished == nil {
		return
	}
	select {
	case mc.finished <- struct{}{}:
		mc.watching = false
	case <-mc.closech:
	}
}

// Ping implements driver.Pinger interface
func (mc *mysqlConn) Ping(ctx context.Context) (err error) {
	if mc.closed.IsSet() {
		errLog.Print(ErrInvalidConn)
		return driver.ErrBadConn
	}

	var spanChild trace.Span
	ctx, spanChild = mc.beginTracing(ctx, "mysql.Ping")
	defer mc.finishTracing(spanChild)

	if err = mc.watchCancel(ctx); err != nil {
		return
	}
	defer mc.finish()

	if err = mc.writeCommandPacket(comPing); err != nil {
		return mc.markBadConn(err)
	}

	return mc.readResultOK()
}

// BeginTx implements driver.ConnBeginTx interface
func (mc *mysqlConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if mc.closed.IsSet() {
		return nil, driver.ErrBadConn
	}

	var spanChild trace.Span
	ctx, spanChild = mc.beginTracing(ctx, "mysql.BeginTx")
	defer mc.finishTracing(spanChild)

	if err := mc.watchCancel(ctx); err != nil {
		return nil, err
	}
	defer mc.finish()

	if sql.IsolationLevel(opts.Isolation) != sql.LevelDefault {
		level, err := mapIsolationLevel(opts.Isolation)
		if err != nil {
			return nil, err
		}
		err = mc.exec("SET TRANSACTION ISOLATION LEVEL " + level)
		if err != nil {
			return nil, err
		}
	}

	return mc.begin(opts.ReadOnly)
}

func (mc *mysqlConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	err := checkNamedValue(args)
	if err != nil {
		return nil, err
	}

	var spanChild trace.Span
	ctx, spanChild = mc.beginTracing(ctx, "mysql.QueryContext", "query", query)
	defer mc.finishTracing(spanChild)

	if err = mc.watchCancel(ctx); err != nil {
		return nil, err
	}

	rows, err1 := mc.query(query, len(args), func(i int) driver.Value { return args[i].Value })
	if err1 != nil {
		mc.finish()
		return nil, err1
	}
	rows.finish = mc.finish
	return rows, err
}

func (mc *mysqlConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	err := checkNamedValue(args)
	if err != nil {
		return nil, err
	}

	var spanChild trace.Span
	ctx, spanChild = mc.beginTracing(ctx, "mysql.ExecContext", "query", query)
	defer mc.finishTracing(spanChild)

	if err = mc.watchCancel(ctx); err != nil {
		return nil, err
	}
	defer mc.finish()

	return mc.execWithArgs(query, len(args), func(i int) driver.Value { return args[i].Value })
}

func (mc *mysqlConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := mc.watchCancel(ctx); err != nil {
		return nil, err
	}

	var spanChild trace.Span
	ctx, spanChild = mc.beginTracing(ctx, "mysql.PrepareContext", "query", query)
	defer mc.finishTracing(spanChild)

	stmt, err := mc.Prepare(query)
	mc.finish()
	if err != nil {
		return nil, err
	}

	select {
	default:
	case <-ctx.Done():
		_ = stmt.Close()
		return nil, ctx.Err()
	}
	return stmt, nil
}

func (stmt *mysqlStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	err := checkNamedValue(args)
	if err != nil {
		return nil, err
	}

	var spanChild trace.Span
	ctx, spanChild = stmt.mc.beginTracing(ctx, "mysql.stmt.QueryContext")
	defer stmt.mc.finishTracing(spanChild)

	if err = stmt.mc.watchCancel(ctx); err != nil {
		return nil, err
	}

	rows, err1 := stmt.query(len(args), func(i int) driver.Value { return args[i].Value })
	if err1 != nil {
		stmt.mc.finish()
		return nil, err1
	}
	rows.finish = stmt.mc.finish
	return rows, err
}

func (stmt *mysqlStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	err := checkNamedValue(args)
	if err != nil {
		return nil, err
	}

	var spanChild trace.Span
	ctx, spanChild = stmt.mc.beginTracing(ctx, "mysql.stmt.ExecContext")
	defer stmt.mc.finishTracing(spanChild)

	if err = stmt.mc.watchCancel(ctx); err != nil {
		return nil, err
	}
	defer stmt.mc.finish()

	return stmt.exec(len(args), func(i int) driver.Value { return args[i].Value })
}

func (mc *mysqlConn) watchCancel(ctx context.Context) error {
	if mc.watching {
		// Reach here if canceled,
		// so the connection is already invalid
		mc.cleanup()
		return nil
	}
	// When ctx is already cancelled, don't watch it.
	if err := ctx.Err(); err != nil {
		return err
	}
	// When ctx is not cancellable, don't watch it.
	if ctx.Done() == nil {
		return nil
	}
	// When watcher is not alive, can't watch it.
	if mc.watcher == nil {
		return nil
	}

	mc.watching = true
	mc.watcher <- ctx
	return nil
}

func (mc *mysqlConn) startWatcher() {
	watcher := make(chan context.Context, 1)
	mc.watcher = watcher
	finished := make(chan struct{})
	mc.finished = finished
	go func() {
		for {
			var ctx context.Context
			select {
			case ctx = <-watcher:
			case <-mc.closech:
				return
			}

			select {
			case <-ctx.Done():
				mc.cancel(ctx.Err())
			case <-finished:
			case <-mc.closech:
				return
			}
		}
	}()
}

func (mc *mysqlConn) CheckNamedValue(nv *driver.NamedValue) (err error) {
	nv.Value, err = converter{}.ConvertValue(nv.Value)
	return
}

// ResetSession implements driver.SessionResetter.
// (From Go 1.10)
func (mc *mysqlConn) ResetSession(_ context.Context) error {
	if mc.closed.IsSet() {
		return driver.ErrBadConn
	}
	mc.reset = true
	return nil
}

// IsValid implements driver.Validator interface
// (From Go 1.15)
func (mc *mysqlConn) IsValid() bool {
	return !mc.closed.IsSet()
}
