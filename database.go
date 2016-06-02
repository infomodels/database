// Provides methods for creating tables, indexes, and constraints for a data model in a database, and loading data into a database.
package database

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Database represents a database or schema (namespace) within a
// database.
//
// WIP: it's debatable what properties Database should
// have. Considerations: we could use a `*sql.DB` as a property, but
// the easiest and fastest way to load data is to shell out to `psql`,
// which means we need a `DatabaseURL` instead. Likewise, the easiest
// way to perform database operations in a database-agnostic fashion
// is to access a remote `data-models-sqlalchemy` service, so we need
// a `ServiceURL` property in addition to `Model` and `ModelVersion`.
type Database struct {
	Model        string // Model per https://github.com/chop-dbhi/data-models.
	ModelVersion string // Model version per https://github.com/chop-dbhi/data-models.
	DatabaseUrl  string // Better would be `DB *sql.DB`, but that is not adequate for loading data the way we will do it initially.
	Schema       string // This is needed for PostgreSQL if a suitable search_path is not being set automatically per database or user. This may be a *comma-separated list of schemas*.
	DmsaUrl      string // data-models-sqlalchemy base URL, or "" for the default. The URL should include the database name.

	db            *sql.DB        // Database handle?
	driverName    string         // Derived from the DatabaseUrl
	includeTables *regexp.Regexp // Optional pattern matching table names to include (no others will be processed).
	excludeTables *regexp.Regexp // Optional pattern matching table names to exclude (all others will be processed).
}

// Magic list of pedsnet vocabulary tables, together with the most recent
// version for which this list has been verified.  It is not safe to
// use this list for any other version. A warning will be issued
// if the requested ModelVersion doesn't match pedsnetMinorVersionSupported.
var pedsnetMinorVersionSupported = "2.2"
var pedsnetVocabTablesPat = `^(?:vocabulary|concept|concept_ancestor|concept_class|concept_relationship|concept_synonym|domain|drug_strength|relationship|source_to_concept_map)$`

var defaultDmsaUrl = "https://data-models-sqlalchemy.research.chop.edu/"

func joinUrlPath(base string, path string) string {
	baseHasTrailingSlash := strings.HasSuffix(base, "/")
	pathHasLeadingSlash := strings.HasPrefix(path, "/")
	if (baseHasTrailingSlash && !pathHasLeadingSlash) || (!baseHasTrailingSlash && pathHasLeadingSlash) {
		return base + path
	} else if !baseHasTrailingSlash && !pathHasLeadingSlash {
		return base + "/" + path
	} else {
		return base + path[1:]
	}
}

func openDatabase(driverName string, databaseUrl string) (*sql.DB, error) {
	db, err := sql.Open(driverName, databaseUrl)
	if err != nil {
		return db, fmt.Errorf("Error opening %s: %v", databaseUrl, err)
	}
	if err = db.Ping(); err != nil {
		return nil, fmt.Errorf("Error opening %s: %v", databaseUrl, err)
	}
	return db, nil
}

var cachedIsValidVersion *bool

// isValidModelVersion validates a model and version with the DMSA service (and the service itself)
func isValidModelVersion(model string, version string, dmsaUrl string) (isValid bool, err error) {
	if cachedIsValidVersion != nil {
		isValid = *cachedIsValidVersion
		return
	}

	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		err = fmt.Errorf("Model version must look like X.Y.Z, not '%s'", version)
		return
	}

	// First, test the DMSA service URL itself
	var response *http.Response
	response, err = http.Get(dmsaUrl)
	if err != nil {
		err = fmt.Errorf("Cannot access data-models-sqlalchemy web service at %s: %v", dmsaUrl, err)
		return
	}
	if response.StatusCode != 200 {
		err = fmt.Errorf("Data-models-sqlalchemy web service (%s) returned error response: %v", dmsaUrl, http.StatusText(response.StatusCode))
		return
	}

	// Now check the requested version
	url := joinUrlPath(dmsaUrl, fmt.Sprintf("/%s/%s/ddl/postgresql/tables/", model, version))
	response, err = http.Get(url)
	if err != nil {
		err = fmt.Errorf("Cannot access data-models-sqlalchemy web service at %v: %v", url, err)
		return
	}
	if response.StatusCode != 200 {
		return // Normal "not valid" return: isValid will be false and err will be nil
	}
	// Normal "valid" return
	cachedIsValidVersion = new(bool)
	*cachedIsValidVersion = true
	isValid = *cachedIsValidVersion
	return
}

// checkVersion returns nil if the model/version combination is valid according to DMSA, otherwise an error.
// If the data-models-sqlalchemy web service cannot be reached, or if the version is invalid, an error is returned.
func (d *Database) checkModelAndVersion() error {
	isValid, err := isValidModelVersion(d.Model, d.ModelVersion, d.DmsaUrl)
	if err != nil {
		return err
	}
	if !isValid {
		return fmt.Errorf("Invalid version '%s' of model '%s', according to %s", d.ModelVersion, d.Model, d.DmsaUrl)
	}

	return nil
}

// Transact wraps database activity in a transaction.
// Stolen from http://stackoverflow.com/a/23502629/390663
func transact(db *sql.DB, txFunc func(*sql.Tx, ...interface{}) error, args ...interface{}) (err error) {
	var tx *sql.Tx
	if db != nil {
		tx, err = db.Begin()
		if err != nil {
			return
		}
	}
	defer func() {
		if p := recover(); p != nil {
			switch p := p.(type) {
			case error:
				err = p
			default:
				err = fmt.Errorf("%s", p)
			}
		}
		if err != nil {
			if tx != nil {
				tx.Rollback()
			}
			return
		}
		if tx != nil {
			err = tx.Commit()
		}
	}()
	return txFunc(tx, args...)
}

// execute runs a SQL statement within a transaction `tx` or prints the SQL
// on stdout if tx is nil.  Leading whitespace is stripped, for clean logs.
func executeSQL(tx *sql.Tx, sql string) error {
	sql = strings.TrimSpace(sql)
	if tx == nil {
		fmt.Printf("%s;\n", sql)
	} else {
		//    fmt.Printf("%s;\n", sql)
		if _, err := tx.Exec(sql); err != nil {
			return fmt.Errorf("Error executing SQL: %v: %v", sql, err)
		}
	}
	return nil
}

// normalPatternsType is used for parsing the table from SQL that contains the table name, i.e. everything except drops of indexes.
type normalPatternsType struct {
	table string // Regexp pattern containing capture expression for table name in the SQL, e.g. "CREATE TABLE (\w+)"
}

// mapPatternsType is used for parsing both creation and drop SQL in cases where the table name does not occur in the drop SQL, e.g. dropping of constraints.
type mapPatternsType struct {
	tableCreate  string // Regexp pattern containing capture expression for the table name in the *creation* SQL, e.g. " ON (\w+)"
	entityCreate string // Regexp pattern containing capture expression for the index or constraint name in the *creation* SQL, e.g. "CREATE INDEX (\w+)"
	entityDrop   string // Regexp pattern containing capture expression for the index or constraint name in the *drop* SQL, e.g. "DROP INDEX (\w+)"
}

// rawDmsaSql fetches DMSA SQL for vocab tables.
//
// `ddlOperator` is "ddl" (i.e. create) or "drop".
// `ddlOperand` is "tables", "indexes" or "constraints".
//
// Returns a slice of SQL statement strings and an error.
func rawDmsaSql(d *Database, ddlOperator string, ddlOperand string) (sqlStrings []string, err error) {

	url := joinUrlPath(d.DmsaUrl, fmt.Sprintf("/%s/%s/%s/postgresql/%s/", d.Model, d.ModelVersion, ddlOperator, ddlOperand))
	response, err := http.Get(url)
	if err != nil {
		return sqlStrings, fmt.Errorf("Error getting %v: %v", url, err)
	}
	if response.StatusCode != 200 {
		return sqlStrings, fmt.Errorf("Data-models-sqlalchemy web service (%v) returned error: %v", url, http.StatusText(response.StatusCode))
	}
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return sqlStrings, fmt.Errorf("Error reading body from %v: %v", url, err)
	}
	bodyString := string(body)

	stmts := strings.Split(bodyString, ";")

	for _, stmt := range stmts {
		if strings.Contains(stmt, "version_history") {
			if strings.Contains(stmt, "CREATE TABLE") {
				// Kludge to work around a data-models-sqlalchemy problem; kludge will be benign even after the problem is fixed.
				stmt = strings.Replace(stmt, "dms_version VARCHAR(16)", "dms_version VARCHAR(50)", 1)
			}
		}
		sqlStrings = append(sqlStrings, stmt)
	} // end for all SQL statements
	return
} // end func rawDmsaSql

// OK, how do we want our API to look?
// For normal ops:
// sql, _, err := dmsaSql("ddl", "tables", tablePattern)
// For index/constraint drops:
// _, map, err := dmsaSql("ddl", "indexes", tablePattern)
// sql, _, err := dmsaSql("drop", "indexes", specialPatterns, specialMap)
// Better?
// For normal ops:
// sql, err := dmsaSql("ddl", "tables", tablePattern, nil)
// For index/constraint drops:
// map, err := dmsaSqlMap("ddl", "indexes", tablePattern)
// sql, err := dmsaSql("drop", "indexes", specialPatterns, specialMap)
//

// dmsaSqlMap fetches SQL from DMSA and builds a map of index/constraint name to table name.
//
// `ddlOperator` should be "ddl" (i.e. create)
// `ddlOperand` is "tables", "indexes" or "constraints".
// `patterns` is a `mapPatternsType` (if operator is "drop" and operand is "indexes"), or nil
//
// Returns a map of index/constraint name to table name, and an error. In the case of "table", the map is not useful.
func dmsaSqlMap(d *Database, ddlOperator string, ddlOperand string, patterns mapPatternsType) (indexOrConstraintToTableMap map[string]string, err error) {

	var stmts []string
	indexOrConstraintToTableMap = make(map[string]string)

	stmts, err = rawDmsaSql(d, ddlOperator, ddlOperand)
	if err != nil {
		return
	}

	// tableCreate string,  // Regexp pattern containing capture expression for the table name in the *creation* SQL, e.g. " ON (\w+)"
	tableCreatePattern := regexp.MustCompile(patterns.tableCreate)
	// entityCreate string, // Regexp pattern containing capture expression for the index or constraint name in the *creation* SQL, e.g. "CREATE INDEX (\w+)"
	entityCreatePattern := regexp.MustCompile(patterns.entityCreate)

	for _, stmt := range stmts {

		if !strings.Contains(stmt, "version_history") {

			if tableMatches := tableCreatePattern.FindStringSubmatch(stmt); tableMatches != nil {

				table := tableMatches[1]

				var entityMatches []string
				if entityMatches = entityCreatePattern.FindStringSubmatch(stmt); entityMatches == nil {
					err = fmt.Errorf("patterns.entityCreate `%s` is non-empty but does not match against `%s`", patterns.entityCreate, stmt)
					return
				}

				indexOrConstraintToTableMap[entityMatches[1]] = table
			}
		}
	} // end for all SQL statements
	return
} // end func dmsaSqlMap

// dmsaSql fetches DMSA SQL for the specified DDL operation, honoring Database object includeTables and excludeTables patterns.
//
// `ddlOperator` is "ddl" (i.e. create) or "drop".
// `ddlOperand` is "tables", "indexes" or "constraints".
// `patterns` is either a `normalPatternsType` (in most cases) or a `mapPatternsType` (see above)
//
// If `patterns` is `mapPatternsType`, this triggers an indirect lookup of the table name associated with each SQL statement; this is needed for SQL DDL in which the table name does not occur.
//
// This is a helper function used by the functions to create tables, indexes, and constraints.
// The `version_history`-related statements are included in the generated SQL.
//
// Returns a slice of SQL statement strings, a map of index/constraint name to table name, and an error. In the case of "table", the map is not useful.
func dmsaSql(d *Database, ddlOperator string, ddlOperand string, patterns interface{}) (sqlStrings []string, err error) {

	var stmts []string

	stmts, err = rawDmsaSql(d, ddlOperator, ddlOperand)
	if err != nil {
		return
	}

	var entityToTableMap map[string]string
	var pattern *regexp.Regexp // Regexp pattern containing capture expression for the table name in the *creation* SQL, e.g. " ON (\w+)"

	switch pat := patterns.(type) {
	case mapPatternsType:
		// The entity-name-to-table-name mapping is assumed to implicitly occur in the creation SQL, i.e. "ddl"
		if entityToTableMap, err = dmsaSqlMap(d, "ddl", ddlOperand, pat); err != nil {
			return
		}
		pattern = regexp.MustCompile(pat.entityDrop)
	case normalPatternsType:
		pattern = regexp.MustCompile(pat.table)
	}

	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		shouldInclude := false // Whether to include this SQL statement
		var table string
		if strings.Contains(stmt, "version_history") {
			shouldInclude = true
		} else {
			var submatches []string
			submatches = pattern.FindStringSubmatch(stmt)
			if submatches != nil {
				if entityToTableMap != nil {
					var ok bool
					if table, ok = entityToTableMap[submatches[1]]; !ok {
						err = fmt.Errorf("Failed to look up table name for entity `%s` in SQL `%s`", submatches[1], stmt)
					}
				} else {
					table = submatches[1]
				}
				if d.includeTables != nil {
					if d.includeTables.MatchString(table) {
						shouldInclude = true
					}
				} else if d.excludeTables != nil {
					if !d.excludeTables.MatchString(table) {
						shouldInclude = true
					}
				}
			}
		}
		if shouldInclude {
			sqlStrings = append(sqlStrings, stmt)
		}
	} // end for all SQL statements
	return
} // end func dmsaSql

// operateOnTables does the work for the {Create|Drop}{Tables|Indexes|Constraints} functions.
//
// `args` should consist of the following arguments of type string:
//
//  * a Database object,
//  * the DMSA DDL operation ("ddl" or "drop"),
//  * the DMSA operand ("tables", "indexes", or "constraints",
//  * and a struct containing pattern strings, of type normalPatternsType or mapPatternsType.
//
// See also dmsaSql.
//
// `tx` is a transaction handle suitable for executing SQL statements.
// If tx is nil, SQL is emitted on stdout instead of being executed.
func operateOnTables(tx *sql.Tx, args ...interface{}) error {
	var (
		err          error
		d            *Database = args[0].(*Database)
		ddlOperation           = args[1].(string)
		ddlOperand             = args[2].(string)
		patterns               = args[3]
	)

	var stmts []string
	stmts, err = dmsaSql(d, ddlOperation, ddlOperand, patterns)
	if err != nil {
		return err
	}

	if d.Schema != "" {
		if d.driverName == "postgres" {
			if err = executeSQL(tx, fmt.Sprintf("SET search_path TO %s", d.Schema)); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("Schemas are currently supported only for PostgreSQL")
		}
	}

	for _, stmt := range stmts {
		if err = executeSQL(tx, stmt); err != nil {
			return fmt.Errorf("Error executing SQL: `%v`: %v", stmt, err)
		}
	} // end for all SQL statements
	return nil
} // end func operateOnTables

// driverNameFromUrl returns a driver name (derived from the scheme) from a database URL
func driverNameFromUrl(urlString string) (string, error) {
	url, err := url.Parse(urlString)
	if err != nil {
		return "", fmt.Errorf("Invalid URL '%s': %v", urlString, err)
	}
	if url.Scheme == "postgres" || url.Scheme == "postgresql" {
		return "postgres", nil
	} else {
		return "", fmt.Errorf("Unsupported database scheme '%s'", url.Scheme)
	}
}

// versionMatchesMinorVersion returns true if a version X.Y.Z has X.Y matching a reference version A.B.
func versionMatchesMinorVersion(version string, referenceMinorVersion string) bool {
	parts := strings.Split(version, ".")
	return parts[0]+"."+parts[1] == referenceMinorVersion
}

// Open is the constructor for the Database object; it validates properties and opens a connection to the database.
func Open(model string, modelVersion string, databaseUrl string, schema string, dmsaUrl string, includeTablesPat string, excludeTablesPat string) (*Database, error) {
	var err error

	if dmsaUrl == "" {
		dmsaUrl = defaultDmsaUrl
	}

	if model == "pedsnet-core" {
		model = "pedsnet"
		if excludeTablesPat == "" {
			excludeTablesPat = pedsnetVocabTablesPat
			if !versionMatchesMinorVersion(modelVersion, pedsnetMinorVersionSupported) {
				fmt.Fprintf(os.Stderr, "WARNING: this code only supports the %s version series for the pedsnet model", pedsnetMinorVersionSupported)
			}
		}
	} else if model == "pedsnet-vocab" {
		model = "pedsnet"
		if includeTablesPat == "" {
			includeTablesPat = pedsnetVocabTablesPat
			if !versionMatchesMinorVersion(modelVersion, pedsnetMinorVersionSupported) {
				fmt.Fprintf(os.Stderr, "WARNING: this code only supports the %s version series for the pedsnet model", pedsnetMinorVersionSupported)
			}
		}
	}

	var includeTables, excludeTables *regexp.Regexp

	if includeTablesPat != "" {
		if includeTables, err = regexp.Compile(includeTablesPat); err != nil {
			return nil, fmt.Errorf("Invalid includeTablesPat regexp string '%s': %v", includeTablesPat, err)
		}
	}

	if excludeTablesPat != "" {
		if excludeTables, err = regexp.Compile(excludeTablesPat); err != nil {
			return nil, fmt.Errorf("Invalid excludeTablesPat regexp string '%s': %v", excludeTablesPat, err)
		}
	}

	d := &Database{Model: model, ModelVersion: modelVersion, DatabaseUrl: databaseUrl, Schema: schema, DmsaUrl: dmsaUrl, includeTables: includeTables, excludeTables: excludeTables}

	if err = d.checkModelAndVersion(); err != nil {
		return nil, err
	}

	if d.driverName, err = driverNameFromUrl(databaseUrl); err != nil {
		return nil, err
	}

	if d.db, err = openDatabase(d.driverName, d.DatabaseUrl); err != nil {
		return nil, err
	}

	return d, nil
}

func (d *Database) Close() error {
	if d.db != nil {
		if err := d.db.Close(); err != nil {
			return err
		}
	}
	return nil
}

// CreateTables creates the data model tables.
// DDL SQL is obtained from the data-models-sqlalchemy service, i.e.
// https://data-models-sqlalchemy.research.chop.edu/{Model}/{ModelVersion}/ddl/postgresql/tables/.
func (d *Database) CreateTables() error {

	var tablePattern string
	if d.driverName == "postgres" {
		tablePattern = `CREATE TABLE.* (\w+) \(`
	} else {
		return fmt.Errorf("Unsupported database driver: %s", d.driverName)
	}
	return transact(d.db, operateOnTables, d, "ddl", "tables", normalPatternsType{tablePattern})
}

// CreateIndexes adds indexes to the data model tables.
// SQL for the operation is obtained from the data-models-sqlalchemy service,
// e.g. https://data-models-sqlalchemy.research.chop.edu/{Model}/{ModelVersion}/ddl/postgresql/indexes/.
func (d *Database) CreateIndexes() error {
	var tablePattern string
	if d.driverName == "postgres" {
		tablePattern = `ON (\w+) \(`
	} else {
		return fmt.Errorf("Unsupported database driver: %s", d.driverName)
	}
	return transact(d.db, operateOnTables, d, "ddl", "indexes", normalPatternsType{tablePattern})
}

// CreateConstraints adds integrity constraints to the data model tables.
// SQL for the operation is obtained from the data-models-sqlalchemy service,
// e.g. https://data-models-sqlalchemy.research.chop.edu/{Model}/{ModelVersion}/ddl/postgresql/constraints/.
func (d *Database) CreateConstraints() error {
	var tablePattern string
	if d.driverName == "postgres" {
		tablePattern = `ALTER TABLE (\w+)`
	} else {
		return fmt.Errorf("Unsupported database driver: %s", d.driverName)
	}
	return transact(d.db, operateOnTables, d, "ddl", "constraints", normalPatternsType{tablePattern})
}

// DropTables drops the data model tables.
// Constraints and indexes should already have been dropped.
// SQL for the operation is obtained from the data-models-sqlalchemy service, e.g.
// https://data-models-sqlalchemy.research.chop.edu/{Model}/{ModelVersion}/drop/postgresql/tables/.
func (d *Database) DropTables() error {
	var tablePattern string
	if d.driverName == "postgres" {
		tablePattern = `DROP TABLE.* (\w+)`
	} else {
		return fmt.Errorf("Unsupported database driver: %s", d.driverName)
	}
	return transact(d.db, operateOnTables, d, "drop", "tables", normalPatternsType{tablePattern})
}

// DropIndexes drops indexes from the data model tables.
// For best performance, constraints should be dropped before dropping indexes.
// SQL for the operation is obtained from the data-models-sqlalchemy service,
// e.g. https://data-models-sqlalchemy.research.chop.edu/{Model}/{ModelVersion}/drop/postgresql/indexes/.
func (d *Database) DropIndexes() error {
	var createIndexTableNamePattern, createIndexIndexNamePattern, dropIndexIndexNamePattern string
	if d.driverName == "postgres" {
		createIndexTableNamePattern = ` ON (\w+) \(`
		createIndexIndexNamePattern = `CREATE INDEX (\w+) ON`
		dropIndexIndexNamePattern = `DROP INDEX (\w+)`
	} else {
		return fmt.Errorf("Unsupported database driver: %s", d.driverName)
	}
	return transact(d.db, operateOnTables, d, "drop", "indexes", mapPatternsType{createIndexTableNamePattern, createIndexIndexNamePattern, dropIndexIndexNamePattern})
}

// DropConstraints drops integrity constraints from the data model tables.
// Constraints should be dropped before dropping indexes and tables.
// SQL for the operation is obtained from the data-models-sqlalchemy service,
// e.g. https://data-models-sqlalchemy.research.chop.edu/{Model}/{ModelVersion}/ddl/postgresql/constraints/.
func (d *Database) DropConstraints() error {
	var tablePattern string
	if d.driverName == "postgres" {
		tablePattern = `ALTER TABLE (\w+)`
	} else {
		return fmt.Errorf("Unsupported database driver: %s", d.driverName)
	}
	return transact(d.db, operateOnTables, d, "drop", "constraints", normalPatternsType{tablePattern})
}