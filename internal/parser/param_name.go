package parser

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Lumos-Labs-HQ/flash/internal/utils"
)

func (ti *TypeInferrer) InferParamName(sql string, paramIndex int) string {
	if strings.Contains(sql, "?") {
		if name := inferQuestionContextName(sql, paramIndex); name != "" {
			return name
		}
	}
	if strings.Contains(sql, "?") {
		re := regexp.MustCompile(`(?i)\?\s*=\s*ANY\s*\(\s*(?:\w+\.)*(\w+)\s*\)`)
		for _, match := range re.FindAllStringSubmatchIndex(sql, -1) {
			if strings.Count(sql[:match[0]], "?")+1 == paramIndex {
				return sql[match[2]:match[3]]
			}
		}
	}
	// Resolve placeholders in INSERT value lists and function-wrapped UPDATE
	// assignments before the legacy pattern handlers. Those handlers assume a
	// one-to-one parameter/column position and misname literals or COALESCE.
	if strings.Contains(sql, "?") && (strings.Contains(strings.ToUpper(sql), "INSERT INTO") ||
		strings.Contains(strings.ToUpper(sql), "COALESCE")) {
		if name := inferQuestionParamName(sql, paramIndex); name != "" {
			return name
		}
	}
	if strings.Contains(sql, "?") {
		if name := inferQuestionAnyName(sql, paramIndex); name != "" {
			return name
		}
	}
	// Check for INSERT statement first — collect ALL column names from every INSERT in multi-statement SQL
	insertColRegex := regexp.MustCompile(`(?i)INSERT\s+INTO\s+\S+\s*\(([\s\S]*?)\)\s*VALUES`)
	allInsertCols := []string{}
	for _, match := range insertColRegex.FindAllStringSubmatch(sql, -1) {
		for _, c := range strings.Split(match[1], ",") {
			allInsertCols = append(allInsertCols, strings.TrimSpace(c))
		}
	}
	if paramIndex <= len(allInsertCols) {
		return allInsertCols[paramIndex-1]
	}

	// col = ANY($N) — param name is the column name (it's an array param)
	anyNameRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:\w+\.)?(\w+)\s*=\s*ANY\s*\(\s*\$%d`, paramIndex))
	if match := anyNameRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// $N = ANY(col) or $N = ANY(alias.col) — reverse form, param on left side
	anyRevRe := regexp.MustCompile(fmt.Sprintf(`(?i)\$%d\s*=\s*ANY\s*\(\s*(?:\w+\.)?(\w+)\s*\)`, paramIndex))
	if match := anyRevRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	if strings.Contains(sql, "?") {
		// ? || col or col || ? in SELECT — concatenation prefix/suffix
		concatRe := regexp.MustCompile(`\?\s*\|\|\s*(\w+)`)
		if match := concatRe.FindStringSubmatch(sql); len(match) > 1 {
			beforeMatch := sql[:strings.Index(sql, match[0])]
			if strings.Count(beforeMatch, "?")+1 == paramIndex {
				return match[1] + "_prefix"
			}
		}

		// col = ANY(?) — forward ANY pattern with ? params
		anyFwdQRe := regexp.MustCompile(`(?i)(?:\w+\.)?(\w+)\s*=\s*ANY\s*\(\s*\?\s*\)`)
		if allFwdMatches := anyFwdQRe.FindAllStringSubmatchIndex(sql, -1); len(allFwdMatches) > 0 {
			for _, loc := range allFwdMatches {
				beforeMatch := sql[:loc[0]]
				// Count ? up to the ? inside ANY(...) — the ? is inside the parentheses
				matchStr := sql[loc[0]:loc[1]]
				qMarkPos := strings.Index(matchStr, "?")
				totalBefore := strings.Count(beforeMatch, "?") + strings.Count(matchStr[:qMarkPos], "?") + 1
				if paramIndex == totalBefore {
					return anyFwdQRe.FindStringSubmatch(matchStr)[1]
				}
			}
		}

		// SET clause with ? params: SET col = ?, col2 = ?, col = col + ?
		setColPattern := regexp.MustCompile(`(?i)SET\s+([\s\S]*?)(?:WHERE|$)`)
		if setMatch := setColPattern.FindStringSubmatch(sql); len(setMatch) > 1 {
			setClause := setMatch[1]
			// Match both: direct (col = ?) and counter (col = col + ? or col = col - ?)
			colPattern := regexp.MustCompile(`(?i)(\w+)\s*=\s*(?:\w+\s*[+\-]\s*)?\?`)
			allSetMatches := colPattern.FindAllStringSubmatch(setClause, -1)
			setCols := []string{}
			for _, m := range allSetMatches {
				setCols = append(setCols, m[1])
			}
			if paramIndex <= len(setCols) {
				name := setCols[paramIndex-1]
				// For counter pattern (col = col +/- ?) append _delta
				counterCheck := regexp.MustCompile(fmt.Sprintf(`(?i)%s\s*=\s*\w+\s*[+\-]\s*\?`, regexp.QuoteMeta(name)))
				if counterCheck.MatchString(setClause) {
					return name + "_delta"
				}
				return name
			}
			// Offset index past SET params for WHERE matching
			paramIndex = paramIndex - len(setCols)
		}

		// WHERE clause with ? params
		// Find the LAST/outermost WHERE clause to avoid matching subquery WHERE clauses
		wherePos := strings.LastIndex(strings.ToUpper(sql), "WHERE")
		if wherePos >= 0 {
			// Extract the WHERE clause content after the last WHERE keyword
			afterWhere := sql[wherePos+5:] // skip "WHERE"
			// Trim to the end of the WHERE clause (stop at LIMIT/ORDER/GROUP/HAVING/ALLOW FILTERING)
			endRe := regexp.MustCompile(`(?i)\b(LIMIT|ORDER\s+BY|GROUP\s+BY|HAVING|ALLOW\s+FILTERING)\b`)
			if endLoc := endRe.FindStringIndex(afterWhere); endLoc != nil {
				afterWhere = afterWhere[:endLoc[0]]
			}
			whereClause := strings.TrimSpace(afterWhere)

			// Count ? that appear BEFORE the WHERE clause but AFTER any SET clause
			// (SET ? params are already handled above via paramIndex reduction).
			// This accounts for ? in LATERAL subqueries, inline subqueries, etc.
			beforeWhere := sql[:wherePos]
			paramsBefore := strings.Count(beforeWhere, "?")
			// Subtract SET params that were already handled (paramIndex was already reduced)
			setColPattern2 := regexp.MustCompile(`(?i)SET\s+([\s\S]*?)(?:WHERE|$)`)
			setParamsHandled := 0
			if setMatch2 := setColPattern2.FindStringSubmatch(sql); len(setMatch2) > 1 {
				setParamsHandled = strings.Count(setMatch2[1], "?")
			}
			subqueryParamsBefore := paramsBefore - setParamsHandled
			if subqueryParamsBefore < 0 {
				subqueryParamsBefore = 0
			}
			relParamIndex := paramIndex - subqueryParamsBefore

			// col ILIKE '%' || ? || '%' or col ILIKE ? (concatenated search)
			ilikePattern := regexp.MustCompile(`(?i)(\w+)\s+I?LIKE\s+.*?\?`)
			ilikeMatches := ilikePattern.FindAllStringSubmatch(whereClause, -1)

			colPattern := regexp.MustCompile(`(?i)(?:\w+\.)?(\w+)\s*=\s*\?`)
			matches := colPattern.FindAllStringSubmatch(whereClause, -1)
			if relParamIndex > 0 && relParamIndex <= len(matches) && len(matches[relParamIndex-1]) > 1 {
				return matches[relParamIndex-1][1]
			}

			// ILIKE with ? (including '%' || ? || '%' patterns)
			if len(ilikeMatches) > 0 {
				relIdx := relParamIndex
				// Check if this param falls within an ILIKE pattern
				for _, m := range ilikeMatches {
					if relIdx > 0 && relIdx <= strings.Count(m[0], "?") {
						return m[1]
					}
					relIdx -= strings.Count(m[0], "?")
				}
			}

			// CONTAINS ? pattern
			containsPattern := regexp.MustCompile(`(?i)(\w+)\s+CONTAINS\s+\?`)
			allContains := containsPattern.FindAllStringSubmatch(whereClause, -1)
			// Only match if this paramIndex maps to a CONTAINS position
			if relParamIndex > 0 && relParamIndex <= len(allContains) {
				return allContains[relParamIndex-1][1]
			}
			// Also match >= AND <= BETWEEN-style
			whereParamIndex := relParamIndex - len(matches)
			rangePattern := regexp.MustCompile(`(?i)(\w+)\s*(>=|<=|>|<)\s*\?`)
			rangeMatches := rangePattern.FindAllStringSubmatch(whereClause, -1)
			if whereParamIndex > 0 && whereParamIndex <= len(rangeMatches) {
				return rangeMatches[whereParamIndex-1][1]
			}
		}

		// LIMIT ? — count total params before LIMIT to find if this ? is LIMIT
		if regexp.MustCompile(`(?i)LIMIT\s+\?`).MatchString(sql) {
			beforeLimit := regexp.MustCompile(`(?i)LIMIT\s+\?`).Split(sql, 2)[0]
			totalBefore := strings.Count(beforeLimit, "?")
			if paramIndex == totalBefore+1 {
				return "limit"
			}
		}

		// OFFSET ? — count total params before OFFSET
		if regexp.MustCompile(`(?i)OFFSET\s+\?`).MatchString(sql) {
			beforeOffset := regexp.MustCompile(`(?i)OFFSET\s+\?`).Split(sql, 2)[0]
			totalBefore := strings.Count(beforeOffset, "?")
			if paramIndex == totalBefore+1 {
				return "offset"
			}
		}

		// SET col = col + ? (counter increment) → use the column name
		counterRe := regexp.MustCompile(`(?i)(\w+)\s*=\s*(\w+)\s*\+\s*\?`)
		for _, m := range counterRe.FindAllStringSubmatch(sql, -1) {
			if strings.EqualFold(m[1], m[2]) {
				// Find position of this ? in the SQL
				idx := strings.Index(sql, m[0])
				if idx >= 0 {
					pos := strings.Count(sql[:idx+len(m[0])], "?")
					if paramIndex == pos {
						return m[1]
					}
				}
			}
		}
	}
	if strings.Contains(sql, "?") {
		if name := inferQuestionParamName(sql, paramIndex); name != "" {
			return name
		}
	}

	// $N || col (concatenation prefix)
	concatDollarRe := regexp.MustCompile(fmt.Sprintf(`\$%d\s*\|\|\s*(\w+)`, paramIndex))
	if match := concatDollarRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1] + "_prefix"
	}

	// jsonb_set / json_set function params: jsonb_set(expr, $1, $2) → "path", "value"
	// The first arg to jsonb_set can be complex (e.g. COALESCE(col, '{}'))
	jsonbSetPathRe := regexp.MustCompile(fmt.Sprintf(`(?i)jsonb?_set\s*\(.*\$%d`, paramIndex))
	if jsonbSetPathRe.MatchString(sql) {
		// Find all $N params inside jsonb_set(...)
		jsonbSetFullRe := regexp.MustCompile(`(?i)jsonb?_set\s*\(`)
		if loc := jsonbSetFullRe.FindStringIndex(sql); loc != nil {
			// Extract params after the function name, counting from first $N
			afterFn := sql[loc[1]:]
			paramRe := regexp.MustCompile(`\$(\d+)`)
			params := paramRe.FindAllStringSubmatch(afterFn, -1)
			if len(params) >= 2 {
				if fmt.Sprintf("$%d", paramIndex) == params[0][0] {
					return "path"
				}
				if fmt.Sprintf("$%d", paramIndex) == params[1][0] {
					return "value"
				}
			}
		}
	}

	// jsonb_insert(expr, $1, $2) → "path", "value"
	jsonbInsertPathRe := regexp.MustCompile(fmt.Sprintf(`(?i)jsonb?_insert\s*\(.*\$%d`, paramIndex))
	if jsonbInsertPathRe.MatchString(sql) {
		jsonbInsertFullRe := regexp.MustCompile(`(?i)jsonb?_insert\s*\(`)
		if loc := jsonbInsertFullRe.FindStringIndex(sql); loc != nil {
			afterFn := sql[loc[1]:]
			paramRe := regexp.MustCompile(`\$(\d+)`)
			params := paramRe.FindAllStringSubmatch(afterFn, -1)
			if len(params) >= 2 {
				if fmt.Sprintf("$%d", paramIndex) == params[0][0] {
					return "path"
				}
				if fmt.Sprintf("$%d", paramIndex) == params[1][0] {
					return "value"
				}
			}
		}
	}

	// func(col) = $N or func(col)::type = $N (function-wrapped column comparison)
	funcColRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:WHERE|AND|OR)\s*\(?\s*\w+\s*\(\s*(?:\w+\.)?(\w+)\s*\)(?:::\w+)?\s*=\s*\$%d\b`, paramIndex))
	if match := funcColRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	wherePattern := fmt.Sprintf(`(?i)(?:WHERE|AND|OR)\s*\(?\s*(?:\w+\.)?(\w+)\s*=\s*\$%d\b`, paramIndex)
	whereRe := regexp.MustCompile(wherePattern)
	if match := whereRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	limitPattern := fmt.Sprintf(`(?i)LIMIT\s+\$%d`, paramIndex)
	if matched, _ := regexp.MatchString(limitPattern, sql); matched {
		return "limit"
	}

	offsetPattern := fmt.Sprintf(`(?i)OFFSET\s+\$%d`, paramIndex)
	if matched, _ := regexp.MatchString(offsetPattern, sql); matched {
		return "offset"
	}

	// ILIKE / LIKE / SIMILAR TO
	likePattern := fmt.Sprintf(`(?i)(?:\w+\.)?(\w+)\s+(?:I?LIKE|SIMILAR\s+TO|NOT\s+I?LIKE)\s+\S*\$%d\b`, paramIndex)
	likeRe := regexp.MustCompile(likePattern)
	if match := likeRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}
	// ILIKE/LIKE with concatenation: col ILIKE '%' || $N || '%'
	likeConcatPattern := fmt.Sprintf(`(?i)(?:\w+\.)?(\w+)\s+(?:I?LIKE|NOT\s+I?LIKE)\s+.*?\$%d\b`, paramIndex)
	likeConcatRe := regexp.MustCompile(likeConcatPattern)
	if match := likeConcatRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// Interval expressions: ($N || ' days')::INTERVAL or INTERVAL $N or ($N || ' hours')::INTERVAL
	intervalPattern := fmt.Sprintf(`(?i)\(\s*\$%d\s*\|\|\s*'[^']*'\s*\)\s*::\s*INTERVAL`, paramIndex)
	if regexp.MustCompile(intervalPattern).MatchString(sql) {
		return "days"
	}
	// Also: NOW() - INTERVAL '$N days' or INTERVAL '$N' DAY
	intervalPattern2 := fmt.Sprintf(`(?i)INTERVAL\s+\$%d\b`, paramIndex)
	if regexp.MustCompile(intervalPattern2).MatchString(sql) {
		return "days"
	}

	setPattern := fmt.Sprintf(`(?i)SET\s+(\w+)\s*=\s*\$%d`, paramIndex)
	setRe := regexp.MustCompile(setPattern)
	if match := setRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// SET col = col + $N or SET col = col - $N (counter increment/decrement)
	setCounterRe := regexp.MustCompile(fmt.Sprintf(`(?i)(\w+)\s*=\s*\w+\s*[+\-]\s*\$%d`, paramIndex))
	if match := setCounterRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1] + "_delta"
	}

	// SET col = COALESCE($N, col) — "update if not null" pattern
	setCoalesceRe := regexp.MustCompile(fmt.Sprintf(`(?i)(\w+)\s*=\s*COALESCE\s*\(\s*\$%d\b`, paramIndex))
	if match := setCoalesceRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// Multi-assignment SET: ..., col = $N (not just first SET position)
	setAnyRe := regexp.MustCompile(fmt.Sprintf(`(?i)(\w+)\s*=\s*\$%d\b`, paramIndex))
	if match := setAnyRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	betweenPattern := fmt.Sprintf(`(?i)(\w+)\s+BETWEEN\s+\$%d`, paramIndex)
	betweenRe := regexp.MustCompile(betweenPattern)
	if match := betweenRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1] + "_start"
	}

	betweenEndPattern := fmt.Sprintf(`(?i)BETWEEN\s+\$\d+\s+AND\s+\$%d`, paramIndex)
	if matched, _ := regexp.MatchString(betweenEndPattern, sql); matched {
		betweenStartRe := regexp.MustCompile(`(?i)(\w+)\s+BETWEEN`)
		if match := betweenStartRe.FindStringSubmatch(sql); len(match) > 1 {
			return match[1] + "_end"
		}
	}

	compPattern := fmt.Sprintf(`(?i)(?:WHERE|AND|OR)\s+(?:\w+\.)?(\w+)\s*([<>=!]+)\s*\$%d`, paramIndex)
	compRe := regexp.MustCompile(compPattern)
	if match := compRe.FindStringSubmatch(sql); len(match) > 2 {
		col := match[1]
		op := match[2]
		// Detect range pattern: same col with opposite operator on another param
		otherRangeRe := regexp.MustCompile(fmt.Sprintf(`(?i)%s\s*[<>=!]+\s*\$\d+`, regexp.QuoteMeta(col)))
		if len(otherRangeRe.FindAllString(sql, -1)) > 1 {
			if op == ">=" || op == ">" {
				return col + "_start"
			}
			if op == "<=" || op == "<" {
				return col + "_end"
			}
		}
		return col
	}

	// COALESCE(col, ...) op $N
	coalesceRe := regexp.MustCompile(fmt.Sprintf(`(?i)COALESCE\s*\(\s*(\w+)[^)]*\)\s*[><=!]+\s*\$%d`, paramIndex))
	if match := coalesceRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// col @> $N, col && $N, col || $N (jsonb/array operators)
	jsonbOpRe := regexp.MustCompile(fmt.Sprintf(`(?i)(\w+)\s*(?:@>|&&|\|\|)\s*\$%d`, paramIndex))
	if match := jsonbOpRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// col ->> $N IS NOT NULL — jsonb key existence check, name as "key"
	jsonbKeyRe := regexp.MustCompile(fmt.Sprintf(`(?i)(\w+)\s*->>\s*\$%d`, paramIndex))
	if match := jsonbKeyRe.FindStringSubmatch(sql); len(match) > 1 {
		return "key"
	}

	// $N = ANY(col) or $N = ANY(alias.col)
	anyRe := regexp.MustCompile(fmt.Sprintf(`(?i)\$%d\s*=\s*ANY\s*\(\s*(?:\w+\.)?(\w+)\s*\)`, paramIndex))
	if match := anyRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// array_append(col, $N) / array_remove(col, $N)
	arrFnRe := regexp.MustCompile(fmt.Sprintf(`(?i)array_(?:append|remove)\s*\(\s*(\w+)\s*,\s*\$%d\b`, paramIndex))
	if match := arrFnRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// col->>'key' = $N or col->'key' = $N (jsonb field access)
	arrowRe := regexp.MustCompile(fmt.Sprintf(`(?i)(\w+)->>?\S+\s*=\s*\$%d`, paramIndex))
	if match := arrowRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// id = ANY($N::type) — cast variant
	anyWithCastRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:\w+\.)?(\w+)\s*=\s*ANY\s*\(\s*\$%d`, paramIndex))
	if match := anyWithCastRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// HAVING COUNT(*) > $N or HAVING col op $N
	havingRe := regexp.MustCompile(fmt.Sprintf(`(?i)HAVING\s+(?:\w+\s*\(.*?\)\s*)?(?:(\w+)\s*)?[><=!]+\s*\$%d`, paramIndex))
	if match := havingRe.FindStringSubmatch(sql); len(match) > 1 && match[1] != "" {
		return "count_threshold"
	}
	if regexp.MustCompile(fmt.Sprintf(`(?i)HAVING\s+.*\$%d`, paramIndex)).MatchString(sql) {
		return "count_threshold"
	}

	// WHERE (subquery) > $N — correlated subquery threshold
	// Handles: WHERE (SELECT ...) = $N, WHERE ( SELECT ... ) = $N
	subqueryThresholdRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:WHERE|AND|OR)\s+\(\s*SELECT[\s\S]*?\)\s*[><=!]+\s*\$%d`, paramIndex))
	if subqueryThresholdRe.MatchString(sql) {
		return "count"
	}

	// plainto_tsquery(..., $N) or to_tsquery(..., $N) — full-text search
	tsqueryRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:plainto_tsquery|to_tsquery|phraseto_tsquery)\s*\([^,)]+,\s*\$%d\b`, paramIndex))
	if tsqueryRe.MatchString(sql) {
		return "search_query"
	}

	// WHERE col IN ($1, $2, ...) — name each as col_1, col_2
	inListRe := regexp.MustCompile(`(?i)(?:WHERE|AND|OR)\s+(?:\w+\.)?(\w+)\s+IN\s*\(([^)]+)\)`)
	if inMatch := inListRe.FindStringSubmatch(sql); len(inMatch) > 2 {
		colName := inMatch[1]
		for pos, part := range strings.Split(inMatch[2], ",") {
			if regexp.MustCompile(fmt.Sprintf(`\$%d\b`, paramIndex)).MatchString(strings.TrimSpace(part)) {
				return fmt.Sprintf("%s%d", colName, pos+1)
			}
		}
	}

	// CTE: WHERE alias.col > $N — strip table alias and try col name
	ctePropRe := regexp.MustCompile(fmt.Sprintf(`(?i)(?:WHERE|AND|OR)\s+\w+\.(\w+)\s*[><=!]+\s*\$%d`, paramIndex))
	if match := ctePropRe.FindStringSubmatch(sql); len(match) > 1 {
		return match[1]
	}

	// SET col = ROW($1, $2, ...) — name params as col_field1, col_field2
	rowSetRe := regexp.MustCompile(`(?i)SET\s+(\w+)\s*=\s*ROW\s*\(([^)]+)\)`)
	if rowSetMatch := rowSetRe.FindStringSubmatch(sql); len(rowSetMatch) > 2 {
		colName := rowSetMatch[1]
		for pos, part := range strings.Split(rowSetMatch[2], ",") {
			if regexp.MustCompile(fmt.Sprintf(`\$%d\b`, paramIndex)).MatchString(strings.TrimSpace(part)) {
				return fmt.Sprintf("%s_field%d", colName, pos+1)
			}
		}
	}

	// Generic: col = $N anywhere in the SQL (catches multi-column SET clauses etc.)
	genericColRe := regexp.MustCompile(fmt.Sprintf(`(?i)(\w+)\s*=\s*\$%d\b`, paramIndex))
	if match := genericColRe.FindStringSubmatch(sql); len(match) > 1 {
		name := strings.ToLower(match[1])
		// Avoid returning SQL keywords as param names
		if name != "true" && name != "false" && name != "null" {
			return match[1]
		}
	}

	return fmt.Sprintf("param%d", paramIndex)
}

// inferQuestionContextName resolves placeholders hidden inside common SQL
// wrappers (lower/trim/coalesce) and nullable-id predicates. It is positional
// so it remains correct when a query contains multiple wrapped expressions.
func inferQuestionContextName(sql string, paramIndex int) string {
	// Optional filters repeat the same value twice ("? IS NULL OR col = ?").
	// Associate both placeholders with the comparison column, regardless of the
	// operator or qualification. This prevents the first placeholder from being
	// rendered as the SQL token `NULL` (n_u_l_l in generated Rust).
	optional := regexp.MustCompile(`(?i)\?\s+IS\s+NULL\s+OR\s+(?:\w+\.)?(\w+)\s*(?:=|!=|<>|>=|<=|>|<)\s*\?`)
	position := questionParamPosition(sql, paramIndex)
	for _, match := range optional.FindAllStringSubmatchIndex(sql, -1) {
		first := strings.Index(sql[match[0]:match[1]], "?")
		second := strings.LastIndex(sql[match[0]:match[1]], "?")
		if (first >= 0 && position == match[0]+first) || (second >= 0 && position == match[0]+second) {
			return sql[match[2]:match[3]]
		}
	}

	// Function-wrapped comparisons (lower(trim(host, '.')) = lower(trim(?, '.')))
	// need the underlying column rather than a generic parameter name. Parse the
	// expression immediately to the left of the comparison and select its first
	// non-function identifier (host/path/etc.).
	if position >= 0 {
		clause := sql[:position]
		clauseStart := lastLogicalBoundary(clause)
		clause = clause[clauseStart:]
		if op := lastComparisonOperator(clause); op >= 0 {
			left := clause[:op]
			leftTrimmed := strings.TrimSpace(left)
			if strings.HasSuffix(leftTrimmed, ")") {
				if name := wrappedExpressionColumn(left); name != "" {
					return name
				}
			}
		}
	}

	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(\w+)\s*=\s*(?:lower|upper|trim|rtrim|ltrim)\s*\(\s*\?`),
		regexp.MustCompile(`(?i)(?:lower|upper|trim|rtrim|ltrim)\s*\(\s*(\w+)\s*\)\s*=\s*(?:lower|upper|trim|rtrim|ltrim)\s*\(\s*\?`),
		regexp.MustCompile(`(?i)(\w+)\s*(?:!=|<>|>=|<=|>|<)\s*\?`),
	}
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringSubmatchIndex(sql, -1) {
			question := strings.Index(sql[match[0]:match[1]], "?")
			if question < 0 {
				continue
			}
			questionPos := match[0] + question
			if questionParamPosition(sql, paramIndex) == questionPos {
				return sql[match[2]:match[3]]
			}
		}
	}
	return ""
}

func lastLogicalBoundary(s string) int {
	boundary := regexp.MustCompile(`(?i)\b(?:WHERE|AND|OR)\b`)
	last := 0
	for _, loc := range boundary.FindAllStringIndex(s, -1) {
		last = loc[1]
	}
	return last
}

func lastComparisonOperator(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '=' || s[i] == '>' || s[i] == '<' {
			// Skip a comparison operator that belongs to a nested expression.
			return i
		}
	}
	return -1
}

func wrappedExpressionColumn(expr string) string {
	// Ignore quoted literals and collect identifiers that are not function names.
	expr = regexp.MustCompile(`'([^']|'')*'`).ReplaceAllString(expr, " ")
	ident := regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_]*)\b\s*(\()?`)
	functions := map[string]bool{"lower": true, "upper": true, "trim": true, "rtrim": true, "ltrim": true, "coalesce": true, "nullif": true, "cast": true}
	keywords := map[string]bool{"and": true, "or": true, "where": true, "case": true, "when": true, "then": true, "else": true, "end": true, "null": true}
	for _, m := range ident.FindAllStringSubmatch(expr, -1) {
		name := strings.ToLower(m[1])
		if m[2] == "(" || functions[name] || keywords[name] {
			continue
		}
		// A qualified name's first identifier is an alias, not the column.
		if strings.Contains(expr, m[1]+".") {
			continue
		}
		return m[1]
	}
	return ""
}

func inferQuestionAnyName(sql string, paramIndex int) string {
	re := regexp.MustCompile(`(?i)\?\s*=\s*ANY\s*\(\s*(?:\w+\.)*(\w+)\s*\)`)
	for _, match := range re.FindAllStringSubmatchIndex(sql, -1) {
		if strings.Count(sql[:match[0]], "?")+1 == paramIndex {
			return sql[match[2]:match[3]]
		}
	}
	return ""
}

func inferQuestionParamName(sql string, paramIndex int) string {
	position := questionParamPosition(sql, paramIndex)
	if position < 0 {
		return ""
	}

	// INSERT columns must be mapped to the VALUES expression containing the
	// placeholder; literals and function calls may mean column N is not param N.
	insertRe := regexp.MustCompile(`(?i)INSERT\s+INTO\s+\S+\s*\(([^)]+)\)\s*VALUES\s*\(`)
	if match := insertRe.FindStringSubmatchIndex(sql); len(match) >= 4 {
		valuesOpen := match[1] - 1
		valuesEnd := findMatchingParen(sql, valuesOpen)
		if position > valuesOpen && valuesEnd > position {
			columns := utils.SplitColumns(sql[match[2]:match[3]])
			values := utils.SplitColumns(sql[valuesOpen+1 : valuesEnd])
			offset := valuesOpen + 1
			for i, value := range values {
				start := strings.Index(sql[offset:valuesEnd], value)
				if start < 0 {
					continue
				}
				start += offset
				end := start + len(value)
				if position >= start && position < end && i < len(columns) {
					return strings.Trim(strings.TrimSpace(columns[i]), `"'`)
				}
				offset = end
			}
		}
	}

	// UPDATE assignments can wrap placeholders in COALESCE/functions. Resolve
	// the assignment containing this exact placeholder rather than only `col=?`.
	setRe := regexp.MustCompile(`(?i)\bSET\b`)
	if setLoc := setRe.FindStringIndex(sql); setLoc != nil && position > setLoc[1] {
		setEnd := len(sql)
		if whereLoc := regexp.MustCompile(`(?i)\bWHERE\b`).FindStringIndex(sql[setLoc[1]:]); whereLoc != nil {
			setEnd = setLoc[1] + whereLoc[0]
		}
		if position < setEnd {
			assignments := utils.SplitColumns(sql[setLoc[1]:setEnd])
			offset := setLoc[1]
			for _, assignment := range assignments {
				start := strings.Index(sql[offset:setEnd], assignment)
				if start < 0 {
					continue
				}
				start += offset
				end := start + len(assignment)
				if position >= start && position < end {
					if equals := strings.Index(assignment, "="); equals > 0 {
						return strings.Trim(strings.TrimSpace(assignment[:equals]), `"'`)
					}
				}
				offset = end
			}
		}
	}

	before := sql[:position]
	after := sql[position+1:]
	leftColumn := regexp.MustCompile(`(?i)(?:\w+\.)?(\w+)\s*(?:=|<>|!=|<=|>=|<|>|I?LIKE|IS)\s*$`)
	if match := leftColumn.FindStringSubmatch(before); len(match) > 1 {
		return match[1]
	}
	rightColumn := regexp.MustCompile(`(?i)^\s*(?:=|<>|!=|<=|>=|<|>|I?LIKE|IS)\s*(?:\w+\.)?(\w+)`)
	if match := rightColumn.FindStringSubmatch(after); len(match) > 1 {
		return match[1]
	}
	return ""
}

func questionParamPosition(sql string, paramIndex int) int {
	index := 0
	inString := false
	for i := 0; i < len(sql); i++ {
		if sql[i] == '\'' {
			if inString && i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				continue
			}
			inString = !inString
			continue
		}
		if !inString && sql[i] == '?' {
			index++
			if index == paramIndex {
				return i
			}
		}
	}
	return -1
}
