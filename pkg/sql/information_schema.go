// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cockroachdb/cockroach/pkg/docs"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catconstants"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemaexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/sql/vtable"
	"github.com/cockroachdb/errors"
	"golang.org/x/text/collate"
)

const (
	pgCatalogName = sessiondata.PgCatalogName
)

var pgCatalogNameDString = tree.NewDString(pgCatalogName)

// informationSchema lists all the table definitions for
// information_schema.
var informationSchema = virtualSchema{
	name: sessiondata.InformationSchemaName,
	undefinedTables: buildStringSet(
		// Generated with:
		// select distinct '"'||table_name||'",' from information_schema.tables
		//    where table_schema='information_schema' order by table_name;
		"_pg_foreign_data_wrappers",
		"_pg_foreign_servers",
		"_pg_foreign_table_columns",
		"_pg_foreign_tables",
		"_pg_user_mappings",
		"attributes",
		"check_constraint_routine_usage",
		"column_domain_usage",
		"column_options",
		"constraint_table_usage",
		"data_type_privileges",
		"domain_constraints",
		"domain_udt_usage",
		"domains",
		"element_types",
		"foreign_data_wrapper_options",
		"foreign_data_wrappers",
		"foreign_server_options",
		"foreign_servers",
		"foreign_table_options",
		"foreign_tables",
		"information_schema_catalog_name",
		"role_column_grants",
		"role_routine_grants",
		"role_udt_grants",
		"role_usage_grants",
		"routine_privileges",
		"sql_features",
		"sql_implementation_info",
		"sql_languages",
		"sql_packages",
		"sql_parts",
		"sql_sizing",
		"sql_sizing_profiles",
		"transforms",
		"triggered_update_columns",
		"triggers",
		"udt_privileges",
		"usage_privileges",
		"user_defined_types",
		"user_mapping_options",
		"user_mappings",
		"view_column_usage",
		"view_routine_usage",
		"view_table_usage",
	),
	tableDefs: map[descpb.ID]virtualSchemaDef{
		catconstants.InformationSchemaAdministrableRoleAuthorizationsID:  informationSchemaAdministrableRoleAuthorizations,
		catconstants.InformationSchemaApplicableRolesID:                  informationSchemaApplicableRoles,
		catconstants.InformationSchemaCharacterSets:                      informationSchemaCharacterSets,
		catconstants.InformationSchemaCheckConstraints:                   informationSchemaCheckConstraints,
		catconstants.InformationSchemaCollationCharacterSetApplicability: informationSchemaCollationCharacterSetApplicability,
		catconstants.InformationSchemaCollations:                         informationSchemaCollations,
		catconstants.InformationSchemaColumnPrivilegesID:                 informationSchemaColumnPrivileges,
		catconstants.InformationSchemaColumnsTableID:                     informationSchemaColumnsTable,
		catconstants.InformationSchemaColumnUDTUsageID:                   informationSchemaColumnUDTUsage,
		catconstants.InformationSchemaConstraintColumnUsageTableID:       informationSchemaConstraintColumnUsageTable,
		catconstants.InformationSchemaTypePrivilegesID:                   informationSchemaTypePrivilegesTable,
		catconstants.InformationSchemaEnabledRolesID:                     informationSchemaEnabledRoles,
		catconstants.InformationSchemaKeyColumnUsageTableID:              informationSchemaKeyColumnUsageTable,
		catconstants.InformationSchemaParametersTableID:                  informationSchemaParametersTable,
		catconstants.InformationSchemaReferentialConstraintsTableID:      informationSchemaReferentialConstraintsTable,
		catconstants.InformationSchemaRoleTableGrantsID:                  informationSchemaRoleTableGrants,
		catconstants.InformationSchemaRoutineTableID:                     informationSchemaRoutineTable,
		catconstants.InformationSchemaSchemataTableID:                    informationSchemaSchemataTable,
		catconstants.InformationSchemaSchemataTablePrivilegesID:          informationSchemaSchemataTablePrivileges,
		catconstants.InformationSchemaSessionVariables:                   informationSchemaSessionVariables,
		catconstants.InformationSchemaSequencesID:                        informationSchemaSequences,
		catconstants.InformationSchemaStatisticsTableID:                  informationSchemaStatisticsTable,
		catconstants.InformationSchemaTableConstraintTableID:             informationSchemaTableConstraintTable,
		catconstants.InformationSchemaTablePrivilegesID:                  informationSchemaTablePrivileges,
		catconstants.InformationSchemaTablesTableID:                      informationSchemaTablesTable,
		catconstants.InformationSchemaViewsTableID:                       informationSchemaViewsTable,
		catconstants.InformationSchemaUserPrivilegesID:                   informationSchemaUserPrivileges,
	},
	tableValidator:             validateInformationSchemaTable,
	validWithNoDatabaseContext: true,
}

func buildStringSet(ss ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}

var (
	emptyString = tree.NewDString("")
	// information_schema was defined before the BOOLEAN data type was added to
	// the SQL specification. Because of this, boolean values are represented as
	// STRINGs. The BOOLEAN data type should NEVER be used in information_schema
	// tables. Instead, define columns as STRINGs and map bools to STRINGs using
	// yesOrNoDatum.
	yesString = tree.NewDString("YES")
	noString  = tree.NewDString("NO")
)

func yesOrNoDatum(b bool) tree.Datum {
	if b {
		return yesString
	}
	return noString
}

func dNameOrNull(s string) tree.Datum {
	if s == "" {
		return tree.DNull
	}
	return tree.NewDName(s)
}

func dIntFnOrNull(fn func() (int32, bool)) tree.Datum {
	if n, ok := fn(); ok {
		return tree.NewDInt(tree.DInt(n))
	}
	return tree.DNull
}

func validateInformationSchemaTable(table *descpb.TableDescriptor) error {
	// Make sure no tables have boolean columns.
	for i := range table.Columns {
		if table.Columns[i].Type.Family() == types.BoolFamily {
			return errors.Errorf("information_schema tables should never use BOOL columns. "+
				"See the comment about yesOrNoDatum. Found BOOL column in %s.", table.Name)
		}
	}
	return nil
}

var informationSchemaAdministrableRoleAuthorizations = virtualSchemaTable{
	comment: `roles for which the current user has admin option
` + docs.URL("information-schema.html#administrable_role_authorizations") + `
https://www.postgresql.org/docs/9.5/infoschema-administrable-role-authorizations.html`,
	schema: vtable.InformationSchemaAdministrableRoleAuthorizations,
	populate: func(ctx context.Context, p *planner, _ catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		currentUser := p.SessionData().User()
		memberMap, err := p.MemberOfWithAdminOption(ctx, currentUser)
		if err != nil {
			return err
		}

		grantee := tree.NewDString(currentUser.Normalized())
		for roleName, isAdmin := range memberMap {
			if !isAdmin {
				// We only show memberships with the admin option.
				continue
			}

			if err := addRow(
				grantee,                                // grantee: always the current user
				tree.NewDString(roleName.Normalized()), // role_name
				yesString,                              // is_grantable: always YES
			); err != nil {
				return err
			}
		}

		return nil
	},
}

var informationSchemaApplicableRoles = virtualSchemaTable{
	comment: `roles available to the current user
` + docs.URL("information-schema.html#applicable_roles") + `
https://www.postgresql.org/docs/9.5/infoschema-applicable-roles.html`,
	schema: vtable.InformationSchemaApplicableRoles,
	populate: func(ctx context.Context, p *planner, _ catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		currentUser := p.SessionData().User()
		memberMap, err := p.MemberOfWithAdminOption(ctx, currentUser)
		if err != nil {
			return err
		}

		grantee := tree.NewDString(currentUser.Normalized())

		for roleName, isAdmin := range memberMap {
			if err := addRow(
				grantee,                                // grantee: always the current user
				tree.NewDString(roleName.Normalized()), // role_name
				yesOrNoDatum(isAdmin),                  // is_grantable
			); err != nil {
				return err
			}
		}

		return nil
	},
}

var informationSchemaCharacterSets = virtualSchemaTable{
	comment: `character sets available in the current database
` + docs.URL("information-schema.html#character_sets") + `
https://www.postgresql.org/docs/9.5/infoschema-character-sets.html`,
	schema: vtable.InformationSchemaCharacterSets,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachDatabaseDesc(ctx, p, nil /* all databases */, true, /* requiresPrivileges */
			func(db catalog.DatabaseDescriptor) error {
				return addRow(
					tree.DNull,                    // character_set_catalog
					tree.DNull,                    // character_set_schema
					tree.NewDString("UTF8"),       // character_set_name: UTF8 is the only available encoding
					tree.NewDString("UCS"),        // character_repertoire: UCS for UTF8 encoding
					tree.NewDString("UTF8"),       // form_of_use: same as the database encoding
					tree.NewDString(db.GetName()), // default_collate_catalog
					tree.DNull,                    // default_collate_schema
					tree.DNull,                    // default_collate_name
				)
			})
	},
}

var informationSchemaCheckConstraints = virtualSchemaTable{
	comment: `check constraints
` + docs.URL("information-schema.html#check_constraints") + `
https://www.postgresql.org/docs/9.5/infoschema-check-constraints.html`,
	schema: vtable.InformationSchemaCheckConstraints,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		h := makeOidHasher()
		return forEachTableDescWithTableLookup(ctx, p, dbContext, hideVirtual /* no constraints in virtual tables */, func(
			db catalog.DatabaseDescriptor,
			scName string,
			table catalog.TableDescriptor,
			tableLookup tableLookupFn,
		) error {
			conInfo, err := table.GetConstraintInfoWithLookup(tableLookup.getTableByID)
			if err != nil {
				return err
			}
			dbNameStr := tree.NewDString(db.GetName())
			scNameStr := tree.NewDString(scName)
			for conName, con := range conInfo {
				// Only Check constraints are included.
				if con.Kind != descpb.ConstraintTypeCheck {
					continue
				}
				conNameStr := tree.NewDString(conName)
				// Like with pg_catalog.pg_constraint, Postgres wraps the check
				// constraint expression in two pairs of parentheses.
				chkExprStr := tree.NewDString(fmt.Sprintf("((%s))", con.Details))
				if err := addRow(
					dbNameStr,  // constraint_catalog
					scNameStr,  // constraint_schema
					conNameStr, // constraint_name
					chkExprStr, // check_clause
				); err != nil {
					return err
				}
			}

			// Unlike with pg_catalog.pg_constraint, Postgres also includes NOT
			// NULL column constraints in information_schema.check_constraints.
			// Cockroach doesn't track these constraints as check constraints,
			// but we can pull them off of the table's column descriptors.
			for _, column := range table.PublicColumns() {
				// Only visible, non-nullable columns are included.
				if column.IsHidden() || column.IsNullable() {
					continue
				}
				// Generate a unique name for each NOT NULL constraint. Postgres
				// uses the format <namespace_oid>_<table_oid>_<col_idx>_not_null.
				// We might as well do the same.
				conNameStr := tree.NewDString(fmt.Sprintf(
					"%s_%s_%d_not_null", h.NamespaceOid(db.GetID(), scName), tableOid(table.GetID()), column.Ordinal()+1,
				))
				chkExprStr := tree.NewDString(fmt.Sprintf(
					"%s IS NOT NULL", column.GetName(),
				))
				if err := addRow(
					dbNameStr,  // constraint_catalog
					scNameStr,  // constraint_schema
					conNameStr, // constraint_name
					chkExprStr, // check_clause
				); err != nil {
					return err
				}
			}
			return nil
		})
	},
}

var informationSchemaColumnPrivileges = virtualSchemaTable{
	comment: `column privilege grants (incomplete)
` + docs.URL("information-schema.html#column_privileges") + `
https://www.postgresql.org/docs/9.5/infoschema-column-privileges.html`,
	schema: vtable.InformationSchemaColumnPrivileges,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, dbContext, virtualMany, func(
			db catalog.DatabaseDescriptor, scName string, table catalog.TableDescriptor,
		) error {
			dbNameStr := tree.NewDString(db.GetName())
			scNameStr := tree.NewDString(scName)
			columndata := privilege.List{privilege.SELECT, privilege.INSERT, privilege.UPDATE} // privileges for column level granularity
			for _, u := range table.GetPrivileges().Users {
				for _, priv := range columndata {
					if priv.Mask()&u.Privileges != 0 {
						for _, cd := range table.PublicColumns() {
							if err := addRow(
								tree.DNull,                             // grantor
								tree.NewDString(u.User().Normalized()), // grantee
								dbNameStr,                              // table_catalog
								scNameStr,                              // table_schema
								tree.NewDString(table.GetName()),       // table_name
								tree.NewDString(cd.GetName()),          // column_name
								tree.NewDString(priv.String()),         // privilege_type
								tree.DNull,                             // is_grantable
							); err != nil {
								return err
							}
						}
					}
				}
			}
			return nil
		})
	},
}

var informationSchemaColumnsTable = virtualSchemaTable{
	comment: `table and view columns (incomplete)
` + docs.URL("information-schema.html#columns") + `
https://www.postgresql.org/docs/9.5/infoschema-columns.html`,
	schema: vtable.InformationSchemaColumns,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		// Get the collations for all comments of current database.
		comments, err := getComments(ctx, p)
		if err != nil {
			return err
		}
		// Push all comments of columns into map.
		commentMap := make(map[tree.DInt]map[tree.DInt]string)
		for _, comment := range comments {
			objID := tree.MustBeDInt(comment[0])
			objSubID := tree.MustBeDInt(comment[1])
			description := comment[2].String()
			commentType := tree.MustBeDInt(comment[3])
			if commentType == 2 {
				if commentMap[objID] == nil {
					commentMap[objID] = make(map[tree.DInt]string)
				}
				commentMap[objID][objSubID] = description
			}
		}

		return forEachTableDesc(ctx, p, dbContext, virtualMany, func(
			db catalog.DatabaseDescriptor, scName string, table catalog.TableDescriptor,
		) error {
			dbNameStr := tree.NewDString(db.GetName())
			scNameStr := tree.NewDString(scName)
			for _, column := range table.PublicColumns() {
				collationCatalog := tree.DNull
				collationSchema := tree.DNull
				collationName := tree.DNull
				if locale := column.GetType().Locale(); locale != "" {
					collationCatalog = dbNameStr
					collationSchema = pgCatalogNameDString
					collationName = tree.NewDString(locale)
				}
				colDefault := tree.DNull
				if column.HasDefault() {
					colExpr, err := schemaexpr.FormatExprForDisplay(ctx, table, column.GetDefaultExpr(), &p.semaCtx, tree.FmtParsable)
					if err != nil {
						return err
					}
					colDefault = tree.NewDString(colExpr)
				}
				colComputed := emptyString
				if column.IsComputed() {
					colExpr, err := schemaexpr.FormatExprForDisplay(ctx, table, column.GetComputeExpr(), &p.semaCtx, tree.FmtSimple)
					if err != nil {
						return err
					}
					colComputed = tree.NewDString(colExpr)
				}

				// Match the comment belonging to current column from map,using table id and column id
				tableID := tree.DInt(table.GetID())
				columnID := tree.DInt(column.GetID())
				description := commentMap[tableID][columnID]

				// udt_schema is set to pg_catalog for builtin types. If, however, the
				// type is a user defined type, then we should fill this value based on
				// the schema it is under.
				udtSchema := pgCatalogNameDString
				typeMetaName := column.GetType().TypeMeta.Name
				if typeMetaName != nil {
					udtSchema = tree.NewDString(typeMetaName.Schema)
				}

				err := addRow(
					dbNameStr,                         // table_catalog
					scNameStr,                         // table_schema
					tree.NewDString(table.GetName()),  // table_name
					tree.NewDString(column.GetName()), // column_name
					tree.NewDString(description),      // column_comment
					tree.NewDInt(tree.DInt(column.GetPGAttributeNum())), // ordinal_position
					colDefault,                        // column_default
					yesOrNoDatum(column.IsNullable()), // is_nullable
					tree.NewDString(column.GetType().InformationSchemaName()), // data_type
					characterMaximumLength(column.GetType()),                  // character_maximum_length
					characterOctetLength(column.GetType()),                    // character_octet_length
					numericPrecision(column.GetType()),                        // numeric_precision
					numericPrecisionRadix(column.GetType()),                   // numeric_precision_radix
					numericScale(column.GetType()),                            // numeric_scale
					datetimePrecision(column.GetType()),                       // datetime_precision
					tree.DNull,                                                // interval_type
					tree.DNull,                                                // interval_precision
					tree.DNull,                                                // character_set_catalog
					tree.DNull,                                                // character_set_schema
					tree.DNull,                                                // character_set_name
					collationCatalog,                                          // collation_catalog
					collationSchema,                                           // collation_schema
					collationName,                                             // collation_name
					tree.DNull,                                                // domain_catalog
					tree.DNull,                                                // domain_schema
					tree.DNull,                                                // domain_name
					dbNameStr,                                                 // udt_catalog
					udtSchema,                                                 // udt_schema
					tree.NewDString(column.GetType().PGName()), // udt_name
					tree.DNull, // scope_catalog
					tree.DNull, // scope_schema
					tree.DNull, // scope_name
					tree.DNull, // maximum_cardinality
					tree.DNull, // dtd_identifier
					tree.DNull, // is_self_referencing
					//TODO: Need to update when supporting identiy columns (Issue #48532)
					noString,                          // is_identity
					tree.DNull,                        // identity_generation
					tree.DNull,                        // identity_start
					tree.DNull,                        // identity_increment
					tree.DNull,                        // identity_maximum
					tree.DNull,                        // identity_minimum
					tree.DNull,                        // identity_cycle
					yesOrNoDatum(column.IsComputed()), // is_generated
					colComputed,                       // generation_expression
					yesOrNoDatum(table.IsTable() &&
						!table.IsVirtualTable() &&
						!column.IsComputed(),
					), // is_updatable
					yesOrNoDatum(column.IsHidden()),               // is_hidden
					tree.NewDString(column.GetType().SQLString()), // crdb_sql_type
				)
				if err != nil {
					return err
				}
			}
			return nil
		})
	},
}

var informationSchemaColumnUDTUsage = virtualSchemaTable{
	comment: `columns with user defined types
` + docs.URL("information-schema.html#column_udt_usage") + `
https://www.postgresql.org/docs/current/infoschema-column-udt-usage.html`,
	schema: vtable.InformationSchemaColumnUDTUsage,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, dbContext, hideVirtual,
			func(db catalog.DatabaseDescriptor, scName string, table catalog.TableDescriptor) error {
				dbNameStr := tree.NewDString(db.GetName())
				scNameStr := tree.NewDString(scName)
				tbNameStr := tree.NewDString(table.GetName())
				for _, col := range table.PublicColumns() {
					if !col.GetType().UserDefined() {
						continue
					}
					if err := addRow(
						tree.NewDString(col.GetType().TypeMeta.Name.Catalog), // UDT_CATALOG
						tree.NewDString(col.GetType().TypeMeta.Name.Schema),  // UDT_SCHEMA
						tree.NewDString(col.GetType().TypeMeta.Name.Name),    // UDT_NAME
						dbNameStr,                      // TABLE_CATALOG
						scNameStr,                      // TABLE_SCHEMA
						tbNameStr,                      // TABLE_NAME
						tree.NewDString(col.GetName()), // COLUMN_NAME
					); err != nil {
						return err
					}
				}
				return nil
			},
		)
	},
}

var informationSchemaEnabledRoles = virtualSchemaTable{
	comment: `roles for the current user
` + docs.URL("information-schema.html#enabled_roles") + `
https://www.postgresql.org/docs/9.5/infoschema-enabled-roles.html`,
	schema: `
CREATE TABLE information_schema.enabled_roles (
	ROLE_NAME STRING NOT NULL
)`,
	populate: func(ctx context.Context, p *planner, _ catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		currentUser := p.SessionData().User()
		memberMap, err := p.MemberOfWithAdminOption(ctx, currentUser)
		if err != nil {
			return err
		}

		// The current user is always listed.
		if err := addRow(
			tree.NewDString(currentUser.Normalized()), // role_name: the current user
		); err != nil {
			return err
		}

		for roleName := range memberMap {
			if err := addRow(
				tree.NewDString(roleName.Normalized()), // role_name
			); err != nil {
				return err
			}
		}

		return nil
	},
}

// characterMaximumLength returns the declared maximum length of
// characters if the type is a character or bit string data
// type. Returns false if the data type is not a character or bit
// string, or if the string's length is not bounded.
func characterMaximumLength(colType *types.T) tree.Datum {
	return dIntFnOrNull(func() (int32, bool) {
		switch colType.Family() {
		case types.StringFamily, types.CollatedStringFamily, types.BitFamily:
			if colType.Width() > 0 {
				return colType.Width(), true
			}
		}
		return 0, false
	})
}

// characterOctetLength returns the maximum possible length in
// octets of a datum if the T is a character string. Returns
// false if the data type is not a character string, or if the
// string's length is not bounded.
func characterOctetLength(colType *types.T) tree.Datum {
	return dIntFnOrNull(func() (int32, bool) {
		switch colType.Family() {
		case types.StringFamily, types.CollatedStringFamily:
			if colType.Width() > 0 {
				return colType.Width() * utf8.UTFMax, true
			}
		}
		return 0, false
	})
}

// numericPrecision returns the declared or implicit precision of numeric
// data types. Returns false if the data type is not numeric, or if the precision
// of the numeric type is not bounded.
func numericPrecision(colType *types.T) tree.Datum {
	return dIntFnOrNull(func() (int32, bool) {
		switch colType.Family() {
		case types.IntFamily:
			return colType.Width(), true
		case types.FloatFamily:
			if colType.Width() == 32 {
				return 24, true
			}
			return 53, true
		case types.DecimalFamily:
			if colType.Precision() > 0 {
				return colType.Precision(), true
			}
		}
		return 0, false
	})
}

// numericPrecisionRadix returns the implicit precision radix of
// numeric data types. Returns false if the data type is not numeric.
func numericPrecisionRadix(colType *types.T) tree.Datum {
	return dIntFnOrNull(func() (int32, bool) {
		switch colType.Family() {
		case types.IntFamily:
			return 2, true
		case types.FloatFamily:
			return 2, true
		case types.DecimalFamily:
			return 10, true
		}
		return 0, false
	})
}

// NumericScale returns the declared or implicit precision of exact numeric
// data types. Returns false if the data type is not an exact numeric, or if the
// scale of the exact numeric type is not bounded.
func numericScale(colType *types.T) tree.Datum {
	return dIntFnOrNull(func() (int32, bool) {
		switch colType.Family() {
		case types.IntFamily:
			return 0, true
		case types.DecimalFamily:
			if colType.Precision() > 0 {
				return colType.Width(), true
			}
		}
		return 0, false
	})
}

// datetimePrecision returns the declared or implicit precision of Time,
// Timestamp or Interval data types. Returns false if the data type is not
// a Time, Timestamp or Interval.
func datetimePrecision(colType *types.T) tree.Datum {
	return dIntFnOrNull(func() (int32, bool) {
		switch colType.Family() {
		case types.TimeFamily, types.TimeTZFamily, types.TimestampFamily, types.TimestampTZFamily, types.IntervalFamily:
			return colType.Precision(), true
		}
		return 0, false
	})
}

var informationSchemaConstraintColumnUsageTable = virtualSchemaTable{
	comment: `columns usage by constraints
https://www.postgresql.org/docs/9.5/infoschema-constraint-column-usage.html`,
	schema: `
CREATE TABLE information_schema.constraint_column_usage (
	TABLE_CATALOG      STRING NOT NULL,
	TABLE_SCHEMA       STRING NOT NULL,
	TABLE_NAME         STRING NOT NULL,
	COLUMN_NAME        STRING NOT NULL,
	CONSTRAINT_CATALOG STRING NOT NULL,
	CONSTRAINT_SCHEMA  STRING NOT NULL,
	CONSTRAINT_NAME    STRING NOT NULL
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachTableDescWithTableLookup(ctx, p, dbContext, hideVirtual /* no constraints in virtual tables */, func(
			db catalog.DatabaseDescriptor,
			scName string,
			table catalog.TableDescriptor,
			tableLookup tableLookupFn,
		) error {
			conInfo, err := table.GetConstraintInfoWithLookup(tableLookup.getTableByID)
			if err != nil {
				return err
			}
			scNameStr := tree.NewDString(scName)
			dbNameStr := tree.NewDString(db.GetName())

			for conName, con := range conInfo {
				conTable := table
				conCols := con.Columns
				conNameStr := tree.NewDString(conName)
				if con.Kind == descpb.ConstraintTypeFK {
					// For foreign key constraint, constraint_column_usage
					// identifies the table/columns that the foreign key
					// references.
					conTable = tabledesc.NewBuilder(con.ReferencedTable).BuildImmutableTable()
					conCols, err = conTable.NamesForColumnIDs(con.FK.ReferencedColumnIDs)
					if err != nil {
						return err
					}
				}
				tableNameStr := tree.NewDString(conTable.GetName())
				for _, col := range conCols {
					if err := addRow(
						dbNameStr,            // table_catalog
						scNameStr,            // table_schema
						tableNameStr,         // table_name
						tree.NewDString(col), // column_name
						dbNameStr,            // constraint_catalog
						scNameStr,            // constraint_schema
						conNameStr,           // constraint_name
					); err != nil {
						return err
					}
				}
			}
			return nil
		})
	},
}

// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/key-column-usage-table.html
var informationSchemaKeyColumnUsageTable = virtualSchemaTable{
	comment: `column usage by indexes and key constraints
` + docs.URL("information-schema.html#key_column_usage") + `
https://www.postgresql.org/docs/9.5/infoschema-key-column-usage.html`,
	schema: `
CREATE TABLE information_schema.key_column_usage (
	CONSTRAINT_CATALOG STRING NOT NULL,
	CONSTRAINT_SCHEMA  STRING NOT NULL,
	CONSTRAINT_NAME    STRING NOT NULL,
	TABLE_CATALOG      STRING NOT NULL,
	TABLE_SCHEMA       STRING NOT NULL,
	TABLE_NAME         STRING NOT NULL,
	COLUMN_NAME        STRING NOT NULL,
	ORDINAL_POSITION   INT NOT NULL,
	POSITION_IN_UNIQUE_CONSTRAINT INT
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachTableDescWithTableLookup(ctx, p, dbContext, hideVirtual /* no constraints in virtual tables */, func(
			db catalog.DatabaseDescriptor,
			scName string,
			table catalog.TableDescriptor,
			tableLookup tableLookupFn,
		) error {
			conInfo, err := table.GetConstraintInfoWithLookup(tableLookup.getTableByID)
			if err != nil {
				return err
			}
			dbNameStr := tree.NewDString(db.GetName())
			scNameStr := tree.NewDString(scName)
			tbNameStr := tree.NewDString(table.GetName())
			for conName, con := range conInfo {
				// Only Primary Key, Foreign Key, and Unique constraints are included.
				switch con.Kind {
				case descpb.ConstraintTypePK:
				case descpb.ConstraintTypeFK:
				case descpb.ConstraintTypeUnique:
				default:
					continue
				}

				cstNameStr := tree.NewDString(conName)

				for pos, col := range con.Columns {
					ordinalPos := tree.NewDInt(tree.DInt(pos + 1))
					uniquePos := tree.DNull
					if con.Kind == descpb.ConstraintTypeFK {
						uniquePos = ordinalPos
					}
					if err := addRow(
						dbNameStr,            // constraint_catalog
						scNameStr,            // constraint_schema
						cstNameStr,           // constraint_name
						dbNameStr,            // table_catalog
						scNameStr,            // table_schema
						tbNameStr,            // table_name
						tree.NewDString(col), // column_name
						ordinalPos,           // ordinal_position, 1-indexed
						uniquePos,            // position_in_unique_constraint
					); err != nil {
						return err
					}
				}
			}
			return nil
		})
	},
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-parameters.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/parameters-table.html
var informationSchemaParametersTable = virtualSchemaTable{
	comment: `built-in function parameters (empty - introspection not yet supported)
https://www.postgresql.org/docs/9.5/infoschema-parameters.html`,
	schema: `
CREATE TABLE information_schema.parameters (
	SPECIFIC_CATALOG STRING,
	SPECIFIC_SCHEMA STRING,
	SPECIFIC_NAME STRING,
	ORDINAL_POSITION INT,
	PARAMETER_MODE STRING,
	IS_RESULT STRING,
	AS_LOCATOR STRING,
	PARAMETER_NAME STRING,
	DATA_TYPE STRING,
	CHARACTER_MAXIMUM_LENGTH INT,
	CHARACTER_OCTET_LENGTH INT,
	CHARACTER_SET_CATALOG STRING,
	CHARACTER_SET_SCHEMA STRING,
	CHARACTER_SET_NAME STRING,
	COLLATION_CATALOG STRING,
	COLLATION_SCHEMA STRING,
	COLLATION_NAME STRING,
	NUMERIC_PRECISION INT,
	NUMERIC_PRECISION_RADIX INT,
	NUMERIC_SCALE INT,
	DATETIME_PRECISION INT,
	INTERVAL_TYPE STRING,
	INTERVAL_PRECISION INT,
	UDT_CATALOG STRING,
	UDT_SCHEMA STRING,
	UDT_NAME STRING,
	SCOPE_CATALOG STRING,
	SCOPE_SCHEMA STRING,
	SCOPE_NAME STRING,
	MAXIMUM_CARDINALITY INT,
	DTD_IDENTIFIER STRING,
	PARAMETER_DEFAULT STRING
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return nil
	},
	unimplemented: true,
}

var (
	matchOptionFull    = tree.NewDString("FULL")
	matchOptionPartial = tree.NewDString("PARTIAL")
	matchOptionNone    = tree.NewDString("NONE")

	matchOptionMap = map[descpb.ForeignKeyReference_Match]tree.Datum{
		descpb.ForeignKeyReference_SIMPLE:  matchOptionNone,
		descpb.ForeignKeyReference_FULL:    matchOptionFull,
		descpb.ForeignKeyReference_PARTIAL: matchOptionPartial,
	}

	refConstraintRuleNoAction   = tree.NewDString("NO ACTION")
	refConstraintRuleRestrict   = tree.NewDString("RESTRICT")
	refConstraintRuleSetNull    = tree.NewDString("SET NULL")
	refConstraintRuleSetDefault = tree.NewDString("SET DEFAULT")
	refConstraintRuleCascade    = tree.NewDString("CASCADE")
)

func dStringForFKAction(action descpb.ForeignKeyReference_Action) tree.Datum {
	switch action {
	case descpb.ForeignKeyReference_NO_ACTION:
		return refConstraintRuleNoAction
	case descpb.ForeignKeyReference_RESTRICT:
		return refConstraintRuleRestrict
	case descpb.ForeignKeyReference_SET_NULL:
		return refConstraintRuleSetNull
	case descpb.ForeignKeyReference_SET_DEFAULT:
		return refConstraintRuleSetDefault
	case descpb.ForeignKeyReference_CASCADE:
		return refConstraintRuleCascade
	}
	panic(errors.Errorf("unexpected ForeignKeyReference_Action: %v", action))
}

// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/referential-constraints-table.html
var informationSchemaReferentialConstraintsTable = virtualSchemaTable{
	comment: `foreign key constraints
` + docs.URL("information-schema.html#referential_constraints") + `
https://www.postgresql.org/docs/9.5/infoschema-referential-constraints.html`,
	schema: `
CREATE TABLE information_schema.referential_constraints (
	CONSTRAINT_CATALOG        STRING NOT NULL,
	CONSTRAINT_SCHEMA         STRING NOT NULL,
	CONSTRAINT_NAME           STRING NOT NULL,
	UNIQUE_CONSTRAINT_CATALOG STRING NOT NULL,
	UNIQUE_CONSTRAINT_SCHEMA  STRING NOT NULL,
	UNIQUE_CONSTRAINT_NAME    STRING,
	MATCH_OPTION              STRING NOT NULL,
	UPDATE_RULE               STRING NOT NULL,
	DELETE_RULE               STRING NOT NULL,
	TABLE_NAME                STRING NOT NULL,
	REFERENCED_TABLE_NAME     STRING NOT NULL
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachTableDescWithTableLookup(ctx, p, dbContext, hideVirtual /* no constraints in virtual tables */, func(
			db catalog.DatabaseDescriptor,
			scName string,
			table catalog.TableDescriptor,
			tableLookup tableLookupFn,
		) error {
			dbNameStr := tree.NewDString(db.GetName())
			scNameStr := tree.NewDString(scName)
			tbNameStr := tree.NewDString(table.GetName())
			return table.ForeachOutboundFK(func(fk *descpb.ForeignKeyConstraint) error {
				refTable, err := tableLookup.getTableByID(fk.ReferencedTableID)
				if err != nil {
					return err
				}
				var matchType = tree.DNull
				if r, ok := matchOptionMap[fk.Match]; ok {
					matchType = r
				}
				refConstraint, err := tabledesc.FindFKReferencedUniqueConstraint(
					refTable, fk.ReferencedColumnIDs,
				)
				if err != nil {
					return err
				}
				return addRow(
					dbNameStr,                                // constraint_catalog
					scNameStr,                                // constraint_schema
					tree.NewDString(fk.Name),                 // constraint_name
					dbNameStr,                                // unique_constraint_catalog
					scNameStr,                                // unique_constraint_schema
					tree.NewDString(refConstraint.GetName()), // unique_constraint_name
					matchType,                                // match_option
					dStringForFKAction(fk.OnUpdate),          // update_rule
					dStringForFKAction(fk.OnDelete),          // delete_rule
					tbNameStr,                                // table_name
					tree.NewDString(refTable.GetName()),      // referenced_table_name
				)
			})
		})
	},
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-role-table-grants.html
// MySQL:    missing
var informationSchemaRoleTableGrants = virtualSchemaTable{
	comment: `privileges granted on table or views (incomplete; see also information_schema.table_privileges; may contain excess users or roles)
` + docs.URL("information-schema.html#role_table_grants") + `
https://www.postgresql.org/docs/9.5/infoschema-role-table-grants.html`,
	schema: `
CREATE TABLE information_schema.role_table_grants (
	GRANTOR        STRING,
	GRANTEE        STRING NOT NULL,
	TABLE_CATALOG  STRING NOT NULL,
	TABLE_SCHEMA   STRING NOT NULL,
	TABLE_NAME     STRING NOT NULL,
	PRIVILEGE_TYPE STRING NOT NULL,
	IS_GRANTABLE   STRING,
	WITH_HIERARCHY STRING
)`,
	// This is the same as information_schema.table_privileges. In postgres, this virtual table does
	// not show tables with grants provided through PUBLIC, but table_privileges does.
	// Since we don't have the PUBLIC concept, the two virtual tables are identical.
	populate: populateTablePrivileges,
}

// MySQL:    https://dev.mysql.com/doc/mysql-infoschema-excerpt/5.7/en/routines-table.html
var informationSchemaRoutineTable = virtualSchemaTable{
	comment: `built-in functions (empty - introspection not yet supported)
https://www.postgresql.org/docs/9.5/infoschema-routines.html`,
	schema: `
CREATE TABLE information_schema.routines (
	SPECIFIC_CATALOG STRING,
	SPECIFIC_SCHEMA STRING,
	SPECIFIC_NAME STRING,
	ROUTINE_CATALOG STRING,
	ROUTINE_SCHEMA STRING,
	ROUTINE_NAME STRING,
	ROUTINE_TYPE STRING,
	MODULE_CATALOG STRING,
	MODULE_SCHEMA STRING,
	MODULE_NAME STRING,
	UDT_CATALOG STRING,
	UDT_SCHEMA STRING,
	UDT_NAME STRING,
	DATA_TYPE STRING,
	CHARACTER_MAXIMUM_LENGTH INT,
	CHARACTER_OCTET_LENGTH INT,
	CHARACTER_SET_CATALOG STRING,
	CHARACTER_SET_SCHEMA STRING,
	CHARACTER_SET_NAME STRING,
	COLLATION_CATALOG STRING,
	COLLATION_SCHEMA STRING,
	COLLATION_NAME STRING,
	NUMERIC_PRECISION INT,
	NUMERIC_PRECISION_RADIX INT,
	NUMERIC_SCALE INT,
	DATETIME_PRECISION INT,
	INTERVAL_TYPE STRING,
	INTERVAL_PRECISION STRING,
	TYPE_UDT_CATALOG STRING,
	TYPE_UDT_SCHEMA STRING,
	TYPE_UDT_NAME STRING,
	SCOPE_CATALOG STRING,
	SCOPE_NAME STRING,
	MAXIMUM_CARDINALITY INT,
	DTD_IDENTIFIER STRING,
	ROUTINE_BODY STRING,
	ROUTINE_DEFINITION STRING,
	EXTERNAL_NAME STRING,
	EXTERNAL_LANGUAGE STRING,
	PARAMETER_STYLE STRING,
	IS_DETERMINISTIC STRING,
	SQL_DATA_ACCESS STRING,
	IS_NULL_CALL STRING,
	SQL_PATH STRING,
	SCHEMA_LEVEL_ROUTINE STRING,
	MAX_DYNAMIC_RESULT_SETS INT,
	IS_USER_DEFINED_CAST STRING,
	IS_IMPLICITLY_INVOCABLE STRING,
	SECURITY_TYPE STRING,
	TO_SQL_SPECIFIC_CATALOG STRING,
	TO_SQL_SPECIFIC_SCHEMA STRING,
	TO_SQL_SPECIFIC_NAME STRING,
	AS_LOCATOR STRING,
	CREATED  TIMESTAMPTZ,
	LAST_ALTERED TIMESTAMPTZ,
	NEW_SAVEPOINT_LEVEL  STRING,
	IS_UDT_DEPENDENT STRING,
	RESULT_CAST_FROM_DATA_TYPE STRING,
	RESULT_CAST_AS_LOCATOR STRING,
	RESULT_CAST_CHAR_MAX_LENGTH  INT,
	RESULT_CAST_CHAR_OCTET_LENGTH STRING,
	RESULT_CAST_CHAR_SET_CATALOG STRING,
	RESULT_CAST_CHAR_SET_SCHEMA  STRING,
	RESULT_CAST_CHAR_SET_NAME STRING,
	RESULT_CAST_COLLATION_CATALOG STRING,
	RESULT_CAST_COLLATION_SCHEMA STRING,
	RESULT_CAST_COLLATION_NAME STRING,
	RESULT_CAST_NUMERIC_PRECISION INT,
	RESULT_CAST_NUMERIC_PRECISION_RADIX INT,
	RESULT_CAST_NUMERIC_SCALE INT,
	RESULT_CAST_DATETIME_PRECISION STRING,
	RESULT_CAST_INTERVAL_TYPE STRING,
	RESULT_CAST_INTERVAL_PRECISION INT,
	RESULT_CAST_TYPE_UDT_CATALOG STRING,
	RESULT_CAST_TYPE_UDT_SCHEMA  STRING,
	RESULT_CAST_TYPE_UDT_NAME STRING,
	RESULT_CAST_SCOPE_CATALOG STRING,
	RESULT_CAST_SCOPE_SCHEMA STRING,
	RESULT_CAST_SCOPE_NAME STRING,
	RESULT_CAST_MAXIMUM_CARDINALITY INT,
	RESULT_CAST_DTD_IDENTIFIER STRING
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return nil
	},
	unimplemented: true,
}

// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/schemata-table.html
var informationSchemaSchemataTable = virtualSchemaTable{
	comment: `database schemas (may contain schemata without permission)
` + docs.URL("information-schema.html#schemata") + `
https://www.postgresql.org/docs/9.5/infoschema-schemata.html`,
	schema: vtable.InformationSchemaSchemata,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachDatabaseDesc(ctx, p, dbContext, true, /* requiresPrivileges */
			func(db catalog.DatabaseDescriptor) error {
				return forEachSchema(ctx, p, db, func(sc catalog.ResolvedSchema) error {
					return addRow(
						tree.NewDString(db.GetName()), // catalog_name
						tree.NewDString(sc.Name),      // schema_name
						tree.DNull,                    // default_character_set_name
						tree.DNull,                    // sql_path
						yesOrNoDatum(sc.Kind == catalog.SchemaUserDefined), // crdb_is_user_defined
					)
				})
			})
	},
}

// Custom; PostgreSQL has data_type_privileges, which only shows one row per type,
// which may result in confusing semantics for the user compared to this table
// which has one row for each grantee.
var informationSchemaTypePrivilegesTable = virtualSchemaTable{
	comment: `type privileges (incomplete; may contain excess users or roles)
` + docs.URL("information-schema.html#type_privileges"),
	schema: `
CREATE TABLE information_schema.type_privileges (
	GRANTEE         STRING NOT NULL,
	TYPE_CATALOG    STRING NOT NULL,
	TYPE_SCHEMA     STRING NOT NULL,
	TYPE_NAME       STRING NOT NULL,
	PRIVILEGE_TYPE  STRING NOT NULL
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachDatabaseDesc(ctx, p, dbContext, true, /* requiresPrivileges */
			func(db catalog.DatabaseDescriptor) error {
				dbNameStr := tree.NewDString(db.GetName())
				pgCatalogStr := tree.NewDString("pg_catalog")

				// Generate one for each existing type.
				for _, typ := range types.OidToType {
					for _, it := range []struct {
						grantee   *tree.DString
						privilege *tree.DString
					}{
						{tree.NewDString(security.RootUser), tree.NewDString(privilege.ALL.String())},
						{tree.NewDString(security.AdminRole), tree.NewDString(privilege.ALL.String())},
						{tree.NewDString(security.PublicRole), tree.NewDString(privilege.USAGE.String())},
					} {
						typeNameStr := tree.NewDString(typ.Name())
						if err := addRow(
							it.grantee,
							dbNameStr,
							pgCatalogStr,
							typeNameStr,
							it.privilege,
						); err != nil {
							return err
						}
					}
				}

				// And for all user defined types.
				return forEachTypeDesc(ctx, p, db, func(db catalog.DatabaseDescriptor, sc string, typeDesc catalog.TypeDescriptor) error {
					scNameStr := tree.NewDString(sc)
					typeNameStr := tree.NewDString(typeDesc.GetName())
					// TODO(knz): This should filter for the current user, see
					// https://github.com/cockroachdb/cockroach/issues/35572
					privs := typeDesc.GetPrivileges().Show(privilege.Type)
					for _, u := range privs {
						userNameStr := tree.NewDString(u.User.Normalized())
						for _, priv := range u.Privileges {
							if err := addRow(
								userNameStr,           // grantee
								dbNameStr,             // type_catalog
								scNameStr,             // type_schema
								typeNameStr,           // type_name
								tree.NewDString(priv), // privilege_type
							); err != nil {
								return err
							}
						}
					}
					return nil
				})
			})
	},
}

// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/schema-privileges-table.html
var informationSchemaSchemataTablePrivileges = virtualSchemaTable{
	comment: `schema privileges (incomplete; may contain excess users or roles)
` + docs.URL("information-schema.html#schema_privileges"),
	schema: `
CREATE TABLE information_schema.schema_privileges (
	GRANTEE         STRING NOT NULL,
	TABLE_CATALOG   STRING NOT NULL,
	TABLE_SCHEMA    STRING NOT NULL,
	PRIVILEGE_TYPE  STRING NOT NULL,
	IS_GRANTABLE    STRING
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachDatabaseDesc(ctx, p, dbContext, true, /* requiresPrivileges */
			func(db catalog.DatabaseDescriptor) error {
				return forEachSchema(ctx, p, db, func(sc catalog.ResolvedSchema) error {
					var privs []descpb.UserPrivilegeString
					if sc.Kind == catalog.SchemaUserDefined {
						// User defined schemas have their own privileges.
						privs = sc.Desc.GetPrivileges().Show(privilege.Schema)
					} else {
						// Other schemas inherit from the parent database.
						privs = db.GetPrivileges().Show(privilege.Database)
					}
					dbNameStr := tree.NewDString(db.GetName())
					scNameStr := tree.NewDString(sc.Name)
					// TODO(knz): This should filter for the current user, see
					// https://github.com/cockroachdb/cockroach/issues/35572
					for _, u := range privs {
						userNameStr := tree.NewDString(u.User.Normalized())
						for _, priv := range u.Privileges {
							privKind := privilege.ByName[priv]
							// Non-user defined schemas inherit privileges from the database,
							// but the USAGE privilege is conferred by having SELECT privilege
							// on the database. (There is no SELECT privilege on schemas.)
							if sc.Kind != catalog.SchemaUserDefined {
								if privKind == privilege.SELECT {
									priv = privilege.USAGE.String()
								} else if !privilege.SchemaPrivileges.Contains(privKind) {
									continue
								}
							}

							if err := addRow(
								userNameStr,           // grantee
								dbNameStr,             // table_catalog
								scNameStr,             // table_schema
								tree.NewDString(priv), // privilege_type
								tree.DNull,            // is_grantable
							); err != nil {
								return err
							}
						}
					}
					return nil
				})
			})
	},
}

var (
	indexDirectionNA   = tree.NewDString("N/A")
	indexDirectionAsc  = tree.NewDString(descpb.IndexDescriptor_ASC.String())
	indexDirectionDesc = tree.NewDString(descpb.IndexDescriptor_DESC.String())
)

func dStringForIndexDirection(dir descpb.IndexDescriptor_Direction) tree.Datum {
	switch dir {
	case descpb.IndexDescriptor_ASC:
		return indexDirectionAsc
	case descpb.IndexDescriptor_DESC:
		return indexDirectionDesc
	}
	panic("unreachable")
}

var informationSchemaSequences = virtualSchemaTable{
	comment: `sequences
` + docs.URL("information-schema.html#sequences") + `
https://www.postgresql.org/docs/9.5/infoschema-sequences.html`,
	schema: `
CREATE TABLE information_schema.sequences (
    SEQUENCE_CATALOG         STRING NOT NULL,
    SEQUENCE_SCHEMA          STRING NOT NULL,
    SEQUENCE_NAME            STRING NOT NULL,
    DATA_TYPE                STRING NOT NULL,
    NUMERIC_PRECISION        INT NOT NULL,
    NUMERIC_PRECISION_RADIX  INT NOT NULL,
    NUMERIC_SCALE            INT NOT NULL,
    START_VALUE              STRING NOT NULL,
    MINIMUM_VALUE            STRING NOT NULL,
    MAXIMUM_VALUE            STRING NOT NULL,
    INCREMENT                STRING NOT NULL,
    CYCLE_OPTION             STRING NOT NULL
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, dbContext, hideVirtual, /* no sequences in virtual schemas */
			func(db catalog.DatabaseDescriptor, scName string, table catalog.TableDescriptor) error {
				if !table.IsSequence() {
					return nil
				}
				return addRow(
					tree.NewDString(db.GetName()),    // catalog
					tree.NewDString(scName),          // schema
					tree.NewDString(table.GetName()), // name
					tree.NewDString("bigint"),        // type
					tree.NewDInt(64),                 // numeric precision
					tree.NewDInt(2),                  // numeric precision radix
					tree.NewDInt(0),                  // numeric scale
					tree.NewDString(strconv.FormatInt(table.GetSequenceOpts().Start, 10)),     // start value
					tree.NewDString(strconv.FormatInt(table.GetSequenceOpts().MinValue, 10)),  // min value
					tree.NewDString(strconv.FormatInt(table.GetSequenceOpts().MaxValue, 10)),  // max value
					tree.NewDString(strconv.FormatInt(table.GetSequenceOpts().Increment, 10)), // increment
					noString, // cycle
				)
			})
	},
}

// Postgres: missing
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/statistics-table.html
var informationSchemaStatisticsTable = virtualSchemaTable{
	comment: `index metadata and statistics (incomplete)
` + docs.URL("information-schema.html#statistics"),
	schema: `
CREATE TABLE information_schema.statistics (
	TABLE_CATALOG STRING NOT NULL,
	TABLE_SCHEMA  STRING NOT NULL,
	TABLE_NAME    STRING NOT NULL,
	NON_UNIQUE    STRING NOT NULL,
	INDEX_SCHEMA  STRING NOT NULL,
	INDEX_NAME    STRING NOT NULL,
	SEQ_IN_INDEX  INT NOT NULL,
	COLUMN_NAME   STRING NOT NULL,
	"COLLATION"   STRING,
	CARDINALITY   INT,
	DIRECTION     STRING NOT NULL,
	STORING       STRING NOT NULL,
	IMPLICIT      STRING NOT NULL
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, dbContext, hideVirtual, /* virtual tables have no indexes */
			func(db catalog.DatabaseDescriptor, scName string, table catalog.TableDescriptor) error {
				dbNameStr := tree.NewDString(db.GetName())
				scNameStr := tree.NewDString(scName)
				tbNameStr := tree.NewDString(table.GetName())

				appendRow := func(index *descpb.IndexDescriptor, colName string, sequence int,
					direction tree.Datum, isStored, isImplicit bool,
				) error {
					return addRow(
						dbNameStr,                         // table_catalog
						scNameStr,                         // table_schema
						tbNameStr,                         // table_name
						yesOrNoDatum(!index.Unique),       // non_unique
						scNameStr,                         // index_schema
						tree.NewDString(index.Name),       // index_name
						tree.NewDInt(tree.DInt(sequence)), // seq_in_index
						tree.NewDString(colName),          // column_name
						tree.DNull,                        // collation
						tree.DNull,                        // cardinality
						direction,                         // direction
						yesOrNoDatum(isStored),            // storing
						yesOrNoDatum(isImplicit),          // implicit
					)
				}

				return catalog.ForEachIndex(table, catalog.IndexOpts{}, func(index catalog.Index) error {
					// Columns in the primary key that aren't in index.ColumnNames or
					// index.StoreColumnNames are implicit columns in the index.
					var implicitCols map[string]struct{}
					var hasImplicitCols bool
					if index.HasOldStoredColumns() {
						// Old STORING format: implicit columns are extra columns minus stored
						// columns.
						hasImplicitCols = index.NumExtraColumns() > index.NumStoredColumns()
					} else {
						// New STORING format: implicit columns are extra columns.
						hasImplicitCols = index.NumExtraColumns() > 0
					}
					if hasImplicitCols {
						implicitCols = make(map[string]struct{})
						for i := 0; i < table.GetPrimaryIndex().NumColumns(); i++ {
							col := table.GetPrimaryIndex().GetColumnName(i)
							implicitCols[col] = struct{}{}
						}
					}

					sequence := 1
					for i := 0; i < index.NumColumns(); i++ {
						col := index.GetColumnName(i)
						// We add a row for each column of index.
						dir := dStringForIndexDirection(index.GetColumnDirection(i))
						if err := appendRow(
							index.IndexDesc(),
							col,
							sequence,
							dir,
							false,
							i < index.ExplicitColumnStartIdx(),
						); err != nil {
							return err
						}
						sequence++
						delete(implicitCols, col)
					}
					for i := 0; i < index.NumStoredColumns(); i++ {
						col := index.GetStoredColumnName(i)
						// We add a row for each stored column of index.
						if err := appendRow(index.IndexDesc(), col, sequence,
							indexDirectionNA, true, false); err != nil {
							return err
						}
						sequence++
						delete(implicitCols, col)
					}
					if len(implicitCols) > 0 {
						// In order to have the implicit columns reported in a
						// deterministic order, we will add all of them in the
						// same order as they are mentioned in the primary key.
						//
						// Note that simply iterating over implicitCols map
						// produces non-deterministic output.
						for i := 0; i < table.GetPrimaryIndex().NumColumns(); i++ {
							col := table.GetPrimaryIndex().GetColumnName(i)
							if _, isImplicit := implicitCols[col]; isImplicit {
								// We add a row for each implicit column of index.
								if err := appendRow(index.IndexDesc(), col, sequence,
									indexDirectionAsc, false, true); err != nil {
									return err
								}
								sequence++
							}
						}
					}
					return nil
				})
			})
	},
}

// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/table-constraints-table.html
var informationSchemaTableConstraintTable = virtualSchemaTable{
	comment: `table constraints
` + docs.URL("information-schema.html#table_constraints") + `
https://www.postgresql.org/docs/9.5/infoschema-table-constraints.html`,
	schema: `
CREATE TABLE information_schema.table_constraints (
	CONSTRAINT_CATALOG STRING NOT NULL,
	CONSTRAINT_SCHEMA  STRING NOT NULL,
	CONSTRAINT_NAME    STRING NOT NULL,
	TABLE_CATALOG      STRING NOT NULL,
	TABLE_SCHEMA       STRING NOT NULL,
	TABLE_NAME         STRING NOT NULL,
	CONSTRAINT_TYPE    STRING NOT NULL,
	IS_DEFERRABLE      STRING NOT NULL,
	INITIALLY_DEFERRED STRING NOT NULL
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		h := makeOidHasher()
		return forEachTableDescWithTableLookup(ctx, p, dbContext, hideVirtual, /* virtual tables have no constraints */
			func(
				db catalog.DatabaseDescriptor,
				scName string,
				table catalog.TableDescriptor,
				tableLookup tableLookupFn,
			) error {
				conInfo, err := table.GetConstraintInfoWithLookup(tableLookup.getTableByID)
				if err != nil {
					return err
				}

				dbNameStr := tree.NewDString(db.GetName())
				scNameStr := tree.NewDString(scName)
				tbNameStr := tree.NewDString(table.GetName())

				for conName, c := range conInfo {
					if err := addRow(
						dbNameStr,                       // constraint_catalog
						scNameStr,                       // constraint_schema
						tree.NewDString(conName),        // constraint_name
						dbNameStr,                       // table_catalog
						scNameStr,                       // table_schema
						tbNameStr,                       // table_name
						tree.NewDString(string(c.Kind)), // constraint_type
						yesOrNoDatum(false),             // is_deferrable
						yesOrNoDatum(false),             // initially_deferred
					); err != nil {
						return err
					}
				}

				// Unlike with pg_catalog.pg_constraint, Postgres also includes NOT
				// NULL column constraints in information_schema.check_constraints.
				// Cockroach doesn't track these constraints as check constraints,
				// but we can pull them off of the table's column descriptors.
				for _, col := range table.PublicColumns() {
					if col.IsNullable() {
						continue
					}
					// NOT NULL column constraints are implemented as a CHECK in postgres.
					conNameStr := tree.NewDString(fmt.Sprintf(
						"%s_%s_%d_not_null", h.NamespaceOid(db.GetID(), scName), tableOid(table.GetID()), col.Ordinal()+1,
					))
					if err := addRow(
						dbNameStr,                // constraint_catalog
						scNameStr,                // constraint_schema
						conNameStr,               // constraint_name
						dbNameStr,                // table_catalog
						scNameStr,                // table_schema
						tbNameStr,                // table_name
						tree.NewDString("CHECK"), // constraint_type
						yesOrNoDatum(false),      // is_deferrable
						yesOrNoDatum(false),      // initially_deferred
					); err != nil {
						return err
					}
				}
				return nil
			})
	},
}

// Postgres: not provided
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/user-privileges-table.html
// TODO(knz): this introspection facility is of dubious utility.
var informationSchemaUserPrivileges = virtualSchemaTable{
	comment: `grantable privileges (incomplete)`,
	schema: `
CREATE TABLE information_schema.user_privileges (
	GRANTEE        STRING NOT NULL,
	TABLE_CATALOG  STRING NOT NULL,
	PRIVILEGE_TYPE STRING NOT NULL,
	IS_GRANTABLE   STRING
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachDatabaseDesc(ctx, p, dbContext, true, /* requiresPrivileges */
			func(dbDesc catalog.DatabaseDescriptor) error {
				dbNameStr := tree.NewDString(dbDesc.GetName())
				for _, u := range []string{security.RootUser, security.AdminRole} {
					grantee := tree.NewDString(u)
					for _, p := range privilege.GetValidPrivilegesForObject(privilege.Table).SortedNames() {
						if err := addRow(
							grantee,            // grantee
							dbNameStr,          // table_catalog
							tree.NewDString(p), // privilege_type
							tree.DNull,         // is_grantable
						); err != nil {
							return err
						}
					}
				}
				return nil
			})
	},
}

// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/table-privileges-table.html
var informationSchemaTablePrivileges = virtualSchemaTable{
	comment: `privileges granted on table or views (incomplete; may contain excess users or roles)
` + docs.URL("information-schema.html#table_privileges") + `
https://www.postgresql.org/docs/9.5/infoschema-table-privileges.html`,
	schema: `
CREATE TABLE information_schema.table_privileges (
	GRANTOR        STRING,
	GRANTEE        STRING NOT NULL,
	TABLE_CATALOG  STRING NOT NULL,
	TABLE_SCHEMA   STRING NOT NULL,
	TABLE_NAME     STRING NOT NULL,
	PRIVILEGE_TYPE STRING NOT NULL,
	IS_GRANTABLE   STRING,
	WITH_HIERARCHY STRING NOT NULL
)`,
	populate: populateTablePrivileges,
}

// populateTablePrivileges is used to populate both table_privileges and role_table_grants.
func populateTablePrivileges(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	addRow func(...tree.Datum) error,
) error {
	return forEachTableDesc(ctx, p, dbContext, virtualMany,
		func(db catalog.DatabaseDescriptor, scName string, table catalog.TableDescriptor) error {
			dbNameStr := tree.NewDString(db.GetName())
			scNameStr := tree.NewDString(scName)
			tbNameStr := tree.NewDString(table.GetName())
			// TODO(knz): This should filter for the current user, see
			// https://github.com/cockroachdb/cockroach/issues/35572
			for _, u := range table.GetPrivileges().Show(privilege.Table) {
				for _, priv := range u.Privileges {
					if err := addRow(
						tree.DNull,                           // grantor
						tree.NewDString(u.User.Normalized()), // grantee
						dbNameStr,                            // table_catalog
						scNameStr,                            // table_schema
						tbNameStr,                            // table_name
						tree.NewDString(priv),                // privilege_type
						tree.DNull,                           // is_grantable
						yesOrNoDatum(priv == "SELECT"),       // with_hierarchy
					); err != nil {
						return err
					}
				}
			}
			return nil
		})
}

var (
	tableTypeSystemView = tree.NewDString("SYSTEM VIEW")
	tableTypeBaseTable  = tree.NewDString("BASE TABLE")
	tableTypeView       = tree.NewDString("VIEW")
	tableTypeTemporary  = tree.NewDString("LOCAL TEMPORARY")
)

var informationSchemaTablesTable = virtualSchemaTable{
	comment: `tables and views
` + docs.URL("information-schema.html#tables") + `
https://www.postgresql.org/docs/9.5/infoschema-tables.html`,
	schema: vtable.InformationSchemaTables,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, dbContext, virtualMany, addTablesTableRow(addRow))
	},
}

func addTablesTableRow(
	addRow func(...tree.Datum) error,
) func(
	db catalog.DatabaseDescriptor,
	scName string,
	table catalog.TableDescriptor,
) error {
	return func(db catalog.DatabaseDescriptor, scName string, table catalog.TableDescriptor) error {
		if table.IsSequence() {
			return nil
		}
		tableType := tableTypeBaseTable
		insertable := yesString
		if table.IsVirtualTable() {
			tableType = tableTypeSystemView
			insertable = noString
		} else if table.IsView() {
			tableType = tableTypeView
			insertable = noString
		} else if table.IsTemporary() {
			tableType = tableTypeTemporary
		}
		dbNameStr := tree.NewDString(db.GetName())
		scNameStr := tree.NewDString(scName)
		tbNameStr := tree.NewDString(table.GetName())
		return addRow(
			dbNameStr,  // table_catalog
			scNameStr,  // table_schema
			tbNameStr,  // table_name
			tableType,  // table_type
			insertable, // is_insertable_into
			tree.NewDInt(tree.DInt(table.GetVersion())), // version
		)
	}
}

// Postgres: https://www.postgresql.org/docs/9.6/static/infoschema-views.html
// MySQL:    https://dev.mysql.com/doc/refman/5.7/en/views-table.html
var informationSchemaViewsTable = virtualSchemaTable{
	comment: `views (incomplete)
` + docs.URL("information-schema.html#views") + `
https://www.postgresql.org/docs/9.5/infoschema-views.html`,
	schema: `
CREATE TABLE information_schema.views (
    TABLE_CATALOG              STRING NOT NULL,
    TABLE_SCHEMA               STRING NOT NULL,
    TABLE_NAME                 STRING NOT NULL,
    VIEW_DEFINITION            STRING NOT NULL,
    CHECK_OPTION               STRING,
    IS_UPDATABLE               STRING NOT NULL,
    IS_INSERTABLE_INTO         STRING NOT NULL,
    IS_TRIGGER_UPDATABLE       STRING NOT NULL,
    IS_TRIGGER_DELETABLE       STRING NOT NULL,
    IS_TRIGGER_INSERTABLE_INTO STRING NOT NULL
)`,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		return forEachTableDesc(ctx, p, dbContext, hideVirtual, /* virtual schemas have no views */
			func(db catalog.DatabaseDescriptor, scName string, table catalog.TableDescriptor) error {
				if !table.IsView() {
					return nil
				}
				// Note that the view query printed will not include any column aliases
				// specified outside the initial view query into the definition returned,
				// unlike Postgres. For example, for the view created via
				//  `CREATE VIEW (a) AS SELECT b FROM foo`
				// we'll only print `SELECT b FROM foo` as the view definition here,
				// while Postgres would more accurately print `SELECT b AS a FROM foo`.
				// TODO(a-robinson): Insert column aliases into view query once we
				// have a semantic query representation to work with (#10083).
				return addRow(
					tree.NewDString(db.GetName()),         // table_catalog
					tree.NewDString(scName),               // table_schema
					tree.NewDString(table.GetName()),      // table_name
					tree.NewDString(table.GetViewQuery()), // view_definition
					tree.DNull,                            // check_option
					noString,                              // is_updatable
					noString,                              // is_insertable_into
					noString,                              // is_trigger_updatable
					noString,                              // is_trigger_deletable
					noString,                              // is_trigger_insertable_into
				)
			})
	},
}

// Postgres: https://www.postgresql.org/docs/current/infoschema-collations.html
// MySQL:    https://dev.mysql.com/doc/refman/8.0/en/information-schema-collations-table.html
var informationSchemaCollations = virtualSchemaTable{
	comment: `shows the collations available in the current database
https://www.postgresql.org/docs/current/infoschema-collations.html`,
	schema: vtable.InformationSchemaCollations,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		dbNameStr := tree.NewDString(p.CurrentDatabase())
		add := func(collName string) error {
			return addRow(
				dbNameStr,
				pgCatalogNameDString,
				tree.NewDString(collName),
				// Always NO PAD (The alternative PAD SPACE is not supported.)
				tree.NewDString("NO PAD"),
			)
		}
		if err := add(tree.DefaultCollationTag); err != nil {
			return err
		}
		for _, tag := range collate.Supported() {
			collName := tag.String()
			if err := add(collName); err != nil {
				return err
			}
		}
		return nil
	},
}

// Postgres: https://www.postgresql.org/docs/current/infoschema-collation-character-set-applicab.html
// MySQL:    https://dev.mysql.com/doc/refman/8.0/en/information-schema-collation-character-set-applicability-table.html
var informationSchemaCollationCharacterSetApplicability = virtualSchemaTable{
	comment: `identifies which character set the available collations are 
applicable to. As UTF-8 is the only available encoding this table does not
provide much useful information.
https://www.postgresql.org/docs/current/infoschema-collation-character-set-applicab.html`,
	schema: vtable.InformationSchemaCollationCharacterSetApplicability,
	populate: func(ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		dbNameStr := tree.NewDString(p.CurrentDatabase())
		add := func(collName string) error {
			return addRow(
				dbNameStr,                 // collation_catalog
				pgCatalogNameDString,      // collation_schema
				tree.NewDString(collName), // collation_name
				tree.DNull,                // character_set_catalog
				tree.DNull,                // character_set_schema
				tree.NewDString("UTF8"),   // character_set_name: UTF8 is the only available encoding
			)
		}
		if err := add(tree.DefaultCollationTag); err != nil {
			return err
		}
		for _, tag := range collate.Supported() {
			collName := tag.String()
			if err := add(collName); err != nil {
				return err
			}
		}
		return nil
	},
}

var informationSchemaSessionVariables = virtualSchemaTable{
	comment: `exposes the session variables.`,
	schema:  vtable.InformationSchemaSessionVariables,
	populate: func(ctx context.Context, p *planner, _ catalog.DatabaseDescriptor, addRow func(...tree.Datum) error) error {
		for _, vName := range varNames {
			gen := varGen[vName]
			value := gen.Get(&p.extendedEvalCtx)
			if err := addRow(
				tree.NewDString(vName),
				tree.NewDString(value),
			); err != nil {
				return err
			}
		}
		return nil
	},
}

// forEachSchema iterates over the physical and virtual schemas.
func forEachSchema(
	ctx context.Context,
	p *planner,
	db catalog.DatabaseDescriptor,
	fn func(sc catalog.ResolvedSchema) error,
) error {
	schemaNames, err := getSchemaNames(ctx, p, db)
	if err != nil {
		return err
	}

	vtableEntries := p.getVirtualTabler().getEntries()
	schemas := make([]catalog.ResolvedSchema, 0, len(schemaNames)+len(vtableEntries))
	var userDefinedSchemaIDs []descpb.ID
	for id, name := range schemaNames {
		switch {
		case strings.HasPrefix(name, sessiondata.PgTempSchemaName):
			schemas = append(schemas, catalog.ResolvedSchema{
				Name: name,
				ID:   id,
				Kind: catalog.SchemaTemporary,
			})
		case name == tree.PublicSchema:
			schemas = append(schemas, catalog.ResolvedSchema{
				Name: name,
				ID:   id,
				Kind: catalog.SchemaPublic,
			})
		default:
			// The default case is a user defined schema. Collect the ID to get the
			// descriptor later.
			userDefinedSchemaIDs = append(userDefinedSchemaIDs, id)
		}
	}

	userDefinedSchemas, err := catalogkv.GetSchemaDescriptorsFromIDs(ctx, p.txn, p.ExecCfg().Codec, userDefinedSchemaIDs)
	if err != nil {
		return err
	}
	for i := range userDefinedSchemas {
		desc := userDefinedSchemas[i]
		canSeeDescriptor, err := userCanSeeDescriptor(ctx, p, desc, db, false /* allowAdding */)
		if err != nil {
			return err
		}
		if !canSeeDescriptor {
			continue
		}
		schemas = append(schemas, catalog.ResolvedSchema{
			Name: desc.GetName(),
			ID:   desc.GetID(),
			Kind: catalog.SchemaUserDefined,
			Desc: desc,
		})
	}

	for _, schema := range vtableEntries {
		schemas = append(schemas, catalog.ResolvedSchema{
			Name: schema.desc.GetName(),
			Kind: catalog.SchemaVirtual,
		})
	}

	sort.Slice(schemas, func(i int, j int) bool {
		return schemas[i].Name < schemas[j].Name
	})

	for _, sc := range schemas {
		if err := fn(sc); err != nil {
			return err
		}
	}

	return nil
}

// forEachDatabaseDesc calls a function for the given DatabaseDescriptor, or if
// it is nil, retrieves all database descriptors and iterates through them in
// lexicographical order with respect to their name. If privileges are required,
// the function is only called if the user has privileges on the database.
func forEachDatabaseDesc(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	requiresPrivileges bool,
	fn func(descriptor catalog.DatabaseDescriptor) error,
) error {
	var dbDescs []catalog.DatabaseDescriptor
	if dbContext == nil {
		allDbDescs, err := p.Descriptors().GetAllDatabaseDescriptors(ctx, p.txn)
		if err != nil {
			return err
		}
		dbDescs = allDbDescs
	} else {
		dbDescs = append(dbDescs, dbContext)
	}

	// Ignore databases that the user cannot see.
	for _, dbDesc := range dbDescs {
		canSeeDescriptor := !requiresPrivileges
		if requiresPrivileges {
			var err error
			canSeeDescriptor, err = userCanSeeDescriptor(ctx, p, dbDesc, nil /* parentDBDesc */, false /* allowAdding */)
			if err != nil {
				return err
			}
		}
		if canSeeDescriptor {
			if err := fn(dbDesc); err != nil {
				return err
			}
		}
	}

	return nil
}

// forEachTypeDesc calls a function for each TypeDescriptor. If dbContext is
// not nil, then the function is called for only TypeDescriptors within the
// given database.
func forEachTypeDesc(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	fn func(db catalog.DatabaseDescriptor, sc string, typ catalog.TypeDescriptor) error,
) error {
	descs, err := p.Descriptors().GetAllDescriptors(ctx, p.txn)
	if err != nil {
		return err
	}
	lCtx := newInternalLookupCtx(ctx, descs, dbContext,
		catalogkv.NewOneLevelUncachedDescGetter(p.txn, p.execCfg.Codec))
	for _, id := range lCtx.typIDs {
		typ := lCtx.typDescs[id]
		dbDesc, err := lCtx.getDatabaseByID(typ.GetParentID())
		if err != nil {
			continue
		}
		scName, err := lCtx.getSchemaNameByID(typ.GetParentSchemaID())
		if err != nil {
			return err
		}
		canSeeDescriptor, err := userCanSeeDescriptor(ctx, p, typ, dbDesc, false /* allowAdding */)
		if err != nil {
			return err
		}
		if !canSeeDescriptor {
			continue
		}
		if err := fn(dbDesc, scName, typ); err != nil {
			return err
		}
	}
	return nil
}

// forEachTableDesc retrieves all table descriptors from the current
// database and all system databases and iterates through them. For
// each table, the function will call fn with its respective database
// and table descriptor.
//
// The dbContext argument specifies in which database context we are
// requesting the descriptors. In context nil all descriptors are
// visible, in non-empty contexts only the descriptors of that
// database are visible.
//
// The virtualOpts argument specifies how virtual tables are made
// visible.
func forEachTableDesc(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	virtualOpts virtualOpts,
	fn func(catalog.DatabaseDescriptor, string, catalog.TableDescriptor) error,
) error {
	return forEachTableDescWithTableLookup(ctx, p, dbContext, virtualOpts, func(
		db catalog.DatabaseDescriptor,
		scName string,
		table catalog.TableDescriptor,
		_ tableLookupFn,
	) error {
		return fn(db, scName, table)
	})
}

type virtualOpts int

const (
	// virtualMany iterates over virtual schemas in every catalog/database.
	virtualMany virtualOpts = iota
	// virtualCurrentDB iterates over virtual schemas in the current database.
	virtualCurrentDB
	// hideVirtual completely hides virtual schemas during iteration.
	hideVirtual
)

// forEachTableDescAll does the same as forEachTableDesc but also
// includes newly added non-public descriptors.
func forEachTableDescAll(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	virtualOpts virtualOpts,
	fn func(catalog.DatabaseDescriptor, string, catalog.TableDescriptor) error,
) error {
	return forEachTableDescAllWithTableLookup(ctx, p, dbContext, virtualOpts, func(
		db catalog.DatabaseDescriptor,
		scName string,
		table catalog.TableDescriptor,
		_ tableLookupFn,
	) error {
		return fn(db, scName, table)
	})
}

// forEachTableDescAllWithTableLookup is like forEachTableDescAll, but it also
// provides a tableLookupFn like forEachTableDescWithTableLookup. If validate is
// set to false descriptors will not be validated for existence or consistency
// hence fn should be able to handle nil-s.
func forEachTableDescAllWithTableLookup(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	virtualOpts virtualOpts,
	fn func(catalog.DatabaseDescriptor, string, catalog.TableDescriptor, tableLookupFn) error,
) error {
	return forEachTableDescWithTableLookupInternal(
		ctx, p, dbContext, virtualOpts, true /* allowAdding */, fn,
	)
}

// forEachTableDescWithTableLookup acts like forEachTableDesc, except it also provides a
// tableLookupFn when calling fn to allow callers to lookup fetched table descriptors
// on demand. This is important for callers dealing with objects like foreign keys, where
// the metadata for each object must be augmented by looking at the referenced table.
//
// The dbContext argument specifies in which database context we are
// requesting the descriptors.  In context "" all descriptors are
// visible, in non-empty contexts only the descriptors of that
// database are visible.
func forEachTableDescWithTableLookup(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	virtualOpts virtualOpts,
	fn func(catalog.DatabaseDescriptor, string, catalog.TableDescriptor, tableLookupFn) error,
) error {
	return forEachTableDescWithTableLookupInternal(
		ctx, p, dbContext, virtualOpts, false /* allowAdding */, fn,
	)
}

func getSchemaNames(
	ctx context.Context, p *planner, dbContext catalog.DatabaseDescriptor,
) (map[descpb.ID]string, error) {
	if dbContext != nil {
		return p.Descriptors().GetSchemasForDatabase(ctx, p.txn, dbContext.GetID())
	}
	ret := make(map[descpb.ID]string)
	dbs, err := p.Descriptors().GetAllDatabaseDescriptors(ctx, p.txn)
	if err != nil {
		return nil, err
	}
	for _, db := range dbs {
		if db == nil {
			return nil, catalog.ErrDescriptorNotFound
		}
		schemas, err := p.Descriptors().GetSchemasForDatabase(ctx, p.txn, db.GetID())
		if err != nil {
			return nil, err
		}
		for id, name := range schemas {
			ret[id] = name
		}
	}
	return ret, nil
}

// forEachTableDescWithTableLookupInternal is the logic that supports
// forEachTableDescWithTableLookup.
//
// The allowAdding argument if true includes newly added tables that
// are not yet public.
// The validate argument if false turns off checking if the descriptor ids exist
// and if they are valid.
func forEachTableDescWithTableLookupInternal(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	virtualOpts virtualOpts,
	allowAdding bool,
	fn func(catalog.DatabaseDescriptor, string, catalog.TableDescriptor, tableLookupFn) error,
) error {
	descs, err := p.Descriptors().GetAllDescriptors(ctx, p.txn)
	if err != nil {
		return err
	}
	return forEachTableDescWithTableLookupInternalFromDescriptors(
		ctx, p, dbContext, virtualOpts, allowAdding, descs, fn)
}

func forEachTypeDescWithTableLookupInternalFromDescriptors(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	allowAdding bool,
	descs []catalog.Descriptor,
	fn func(catalog.DatabaseDescriptor, string, catalog.TypeDescriptor, tableLookupFn) error,
) error {
	lCtx := newInternalLookupCtx(ctx, descs, dbContext,
		catalogkv.NewOneLevelUncachedDescGetter(p.txn, p.execCfg.Codec))

	for _, typID := range lCtx.typIDs {
		typDesc := lCtx.typDescs[typID]
		if typDesc.Dropped() {
			continue
		}
		dbDesc, err := lCtx.getDatabaseByID(typDesc.GetParentID())
		if err != nil {
			return err
		}
		canSeeDescriptor, err := userCanSeeDescriptor(ctx, p, typDesc, dbDesc, allowAdding)
		if err != nil {
			return err
		}
		if !canSeeDescriptor {
			continue
		}
		scName, err := lCtx.getSchemaNameByID(typDesc.GetParentSchemaID())
		if err != nil {
			return err
		}
		if err := fn(dbDesc, scName, typDesc, lCtx); err != nil {
			return err
		}
	}
	return nil
}

func forEachTableDescWithTableLookupInternalFromDescriptors(
	ctx context.Context,
	p *planner,
	dbContext catalog.DatabaseDescriptor,
	virtualOpts virtualOpts,
	allowAdding bool,
	descs []catalog.Descriptor,
	fn func(catalog.DatabaseDescriptor, string, catalog.TableDescriptor, tableLookupFn) error,
) error {
	lCtx := newInternalLookupCtx(ctx, descs, dbContext,
		catalogkv.NewOneLevelUncachedDescGetter(p.txn, p.execCfg.Codec))

	if virtualOpts == virtualMany || virtualOpts == virtualCurrentDB {
		// Virtual descriptors first.
		vt := p.getVirtualTabler()
		vEntries := vt.getEntries()
		vSchemaNames := vt.getSchemaNames()
		iterate := func(dbDesc catalog.DatabaseDescriptor) error {
			for _, virtSchemaName := range vSchemaNames {
				e := vEntries[virtSchemaName]
				for _, tName := range e.orderedDefNames {
					te := e.defs[tName]
					if err := fn(dbDesc, virtSchemaName, te.desc, lCtx); err != nil {
						return err
					}
				}
			}
			return nil
		}

		switch virtualOpts {
		case virtualCurrentDB:
			if err := iterate(dbContext); err != nil {
				return err
			}
		case virtualMany:
			for _, dbID := range lCtx.dbIDs {
				dbDesc := lCtx.dbDescs[dbID]
				if err := iterate(dbDesc); err != nil {
					return err
				}
			}
		}
	}

	// Physical descriptors next.
	for _, tbID := range lCtx.tbIDs {
		table := lCtx.tbDescs[tbID]
		dbDesc, parentExists := lCtx.dbDescs[table.GetParentID()]
		canSeeDescriptor, err := userCanSeeDescriptor(ctx, p, table, dbDesc, allowAdding)
		if err != nil {
			return err
		}
		if table.Dropped() || !canSeeDescriptor {
			continue
		}
		var scName string
		if parentExists {
			var ok bool
			scName, ok = lCtx.schemaNames[table.GetParentSchemaID()]
			// Look up the schemas for this database if we discover that there is a
			// missing temporary schema name. The only schemas which do not have
			// descriptors are the public schema and temporary schemas. The public
			// schema does not have a descriptor but will appear in the map. Temporary
			// schemas do, however, have namespace entries. The below code will go
			// and lookup schema names from the namespace table if needed to qualify
			// the name of a temporary table.
			if !ok && !table.IsTemporary() {
				return errors.AssertionFailedf("schema id %d not found", table.GetParentSchemaID())
			}
			if !ok { // && table.IsTemporary()
				namesForSchema, err := getSchemaNames(ctx, p, dbDesc)
				if err != nil {
					return errors.Wrapf(err, "failed to look up schema id %d",
						table.GetParentSchemaID())
				}
				for id, n := range namesForSchema {
					if _, exists := lCtx.schemaNames[id]; exists {
						continue
					}
					lCtx.schemaNames[id] = n
					scName = lCtx.schemaNames[table.GetParentSchemaID()]
				}
			}
		}
		if err := fn(dbDesc, scName, table, lCtx); err != nil {
			return err
		}
	}
	return nil
}

func forEachRole(
	ctx context.Context,
	p *planner,
	fn func(username security.SQLUsername, isRole bool, noLogin bool, rolValidUntil *time.Time) error,
) error {
	query := `
SELECT
	u.username,
	"isRole",
	EXISTS(
		SELECT
			option
		FROM
			system.role_options AS r
		WHERE
			r.username = u.username AND option = 'NOLOGIN'
	)
		AS nologin,
	ro.value::TIMESTAMPTZ AS rolvaliduntil
FROM
	system.users AS u
	LEFT JOIN system.role_options AS ro ON
			ro.username = u.username
			AND option = 'VALID UNTIL';
`
	// For some reason, using the iterator API here causes privilege_builtins
	// logic test fail in 3node-tenant config with 'txn already encountered an
	// error' (because of the context cancellation), so we buffer all roles
	// first.
	rows, err := p.ExtendedEvalContext().ExecCfg.InternalExecutor.QueryBuffered(
		ctx, "read-roles", p.txn, query,
	)
	if err != nil {
		return err
	}

	for _, row := range rows {
		usernameS := tree.MustBeDString(row[0])
		isRole, ok := row[1].(*tree.DBool)
		if !ok {
			return errors.Errorf("isRole should be a boolean value, found %s instead", row[1].ResolvedType())
		}
		noLogin, ok := row[2].(*tree.DBool)
		if !ok {
			return errors.Errorf("noLogin should be a boolean value, found %s instead", row[2].ResolvedType())
		}
		var rolValidUntil *time.Time
		if rolValidUntilDatum, ok := row[3].(*tree.DTimestampTZ); ok {
			rolValidUntil = &rolValidUntilDatum.Time
		} else if row[3] != tree.DNull {
			return errors.Errorf("rolValidUntil should be a timestamp or null value, found %s instead", row[3].ResolvedType())
		}
		// system tables already contain normalized usernames.
		username := security.MakeSQLUsernameFromPreNormalizedString(string(usernameS))
		if err := fn(username, bool(*isRole), bool(*noLogin), rolValidUntil); err != nil {
			return err
		}
	}

	return nil
}

func forEachRoleMembership(
	ctx context.Context, p *planner, fn func(role, member security.SQLUsername, isAdmin bool) error,
) (retErr error) {
	query := `SELECT "role", "member", "isAdmin" FROM system.role_members`
	it, err := p.ExtendedEvalContext().ExecCfg.InternalExecutor.QueryIterator(
		ctx, "read-members", p.txn, query,
	)
	if err != nil {
		return err
	}
	// We have to make sure to close the iterator since we might return from the
	// for loop early (before Next() returns false).
	defer func() { retErr = errors.CombineErrors(retErr, it.Close()) }()

	var ok bool
	for ok, err = it.Next(ctx); ok; ok, err = it.Next(ctx) {
		row := it.Cur()
		roleName := tree.MustBeDString(row[0])
		memberName := tree.MustBeDString(row[1])
		isAdmin := row[2].(*tree.DBool)

		// The names in the system tables are already normalized.
		if err := fn(
			security.MakeSQLUsernameFromPreNormalizedString(string(roleName)),
			security.MakeSQLUsernameFromPreNormalizedString(string(memberName)),
			bool(*isAdmin)); err != nil {
			return err
		}
	}
	return err
}

func userCanSeeDescriptor(
	ctx context.Context, p *planner, desc, parentDBDesc catalog.Descriptor, allowAdding bool,
) (bool, error) {
	if !descriptorIsVisible(desc, allowAdding) {
		return false, nil
	}

	// TODO(richardjcai): We may possibly want to remove the ability to view
	// the descriptor if they have any privilege on the descriptor and only
	// allow the descriptor to be viewed if they have CONNECT on the DB. #59827.
	canSeeDescriptor := p.CheckAnyPrivilege(ctx, desc) == nil
	// Users can see objects in the database if they have connect privilege.
	if parentDBDesc != nil {
		canSeeDescriptor = canSeeDescriptor || p.CheckPrivilege(ctx, parentDBDesc, privilege.CONNECT) == nil
	}
	return canSeeDescriptor, nil
}

func descriptorIsVisible(desc catalog.Descriptor, allowAdding bool) bool {
	return desc.Public() || (allowAdding && desc.Adding())
}
