package v8gen

import (
	"fmt"
	"strings"

	. "github.com/gost-dom/code-gen/internal"
	wrappers "github.com/gost-dom/code-gen/script-wrappers"
	. "github.com/gost-dom/code-gen/script-wrappers/model"
	"github.com/gost-dom/code-gen/stdgen"
	g "github.com/gost-dom/generators"

	"github.com/dave/jennifer/jen"
)

func idlNameToGoName(s string) string {
	words := strings.Split(s, " ")
	for i, word := range words {
		words[i] = UpperCaseFirstLetter(word)
	}
	return strings.Join(words, "")
}

var scriptHost = g.NewValue("scriptHost")

type V8NamingStrategy struct{ ESConstructorData }

func (s V8NamingStrategy) Receiver() string { return "w" }
func (s V8NamingStrategy) PrototypeWrapperBaseName() string {
	return fmt.Sprintf("%sV8Wrapper", s.Name())
}

func (s V8NamingStrategy) PrototypeWrapperName() string {
	return LowerCaseFirstLetter(s.PrototypeWrapperBaseName())
}

func prototypeFactoryFunctionName(data ESConstructorData) string {
	return fmt.Sprintf("create%sPrototype", data.IdlInterfaceName)
}

func CreateV8ConstructorBody(data ESConstructorData) g.Generator {
	naming := V8NamingStrategy{data}
	builder := NewConstructorBuilder()
	constructor := v8FunctionTemplate{g.NewValue("constructor")}

	createWrapperFunction := g.NewValue(fmt.Sprintf("new%s", naming.PrototypeWrapperBaseName()))

	statements := g.StatementList(
		builder.v8Iso.Assign(scriptHost.Field("iso")),
		g.Assign(builder.Wrapper, createWrapperFunction.Call(scriptHost)),
		g.Assign(constructor, builder.NewFunctionTemplateOfWrappedMethod("Constructor")),
		g.Line,
		g.Assign(builder.InstanceTmpl, constructor.GetInstanceTemplate()),
		builder.InstanceTmpl.SetInternalFieldCount(1),
		g.Line,
		builder.Wrapper.Field("installPrototype").Call(constructor.GetPrototypeTemplate()),
		g.Line,
	)
	if data.RunCustomCode {
		statements.Append(
			g.Raw(jen.Id("wrapper").Dot("CustomInitialiser").Call(jen.Id("constructor"))),
		)
	}
	statements.Append(g.Return(constructor))
	return statements
}

func CreateV8ConstructorWrapperBody(data ESConstructorData) g.Generator {
	naming := V8NamingStrategy{data}
	receiver := WrapperInstance{g.NewValue(naming.Receiver())}
	if data.Constructor == nil {
		return CreateV8IllegalConstructorBody(data)
	}
	var readArgsResult V8ReadArguments
	op := *data.Constructor
	readArgsResult = ReadArguments(data, op)
	statements := g.StatementList(
		AssignArgs(data, op),
		readArgsResult)
	statements.Append(V8RequireContext(receiver))
	baseFunctionName := "CreateInstance"
	var CreateCall = func(functionName string, argnames []g.Generator, op ESOperation) g.Generator {
		return g.StatementList(
			g.Return(
				g.Raw(jen.Id(naming.Receiver()).Dot(functionName).CallFunc(func(grp *jen.Group) {
					grp.Add(jen.Id("ctx"))
					grp.Add(jen.Id("info").Dot("This").Call())
					for _, name := range argnames {
						grp.Add(name.Generate())
					}
				})),
			),
		)
	}
	statements.Append(
		CreateV8WrapperMethodInstanceInvocations(
			data,
			op,
			baseFunctionName,
			readArgsResult.Args,
			nil,
			CreateCall,
			false,
		),
	)
	return statements
}

func CreateV8WrapperMethodInstanceInvocations(
	prototype ESConstructorData,
	op ESOperation,
	baseFunctionName string,
	args []V8ReadArg,
	instanceErr g.Generator,
	createCallInstance func(string, []g.Generator, ESOperation) g.Generator,
	extraError bool,
) g.Generator {
	// arguments := op.Arguments
	statements := g.StatementList()
	missingArgsConts := fmt.Sprintf("%s.%s: Missing arguments", prototype.Name(), op.Name)
	for i := len(args); i >= 0; i-- {
		functionName := baseFunctionName
		for j, arg := range args {
			if j < i {
				if arg.Argument.OptionalInGo() {
					functionName += idlNameToGoName(arg.Argument.Name)
				}
			}
		}
		currentArgs := args[0:i]
		ei := i
		if extraError {
			ei++
		}
		errNames := make([]g.Generator, 0, i+1)
		if instanceErr != nil {
			errNames = append(errNames, instanceErr)
		}
		for _, a := range currentArgs {
			errNames = append(errNames, a.ErrName)
		}

		callArgs := make([]g.Generator, i)
		for idx, a := range currentArgs {
			callArgs[idx] = a.ArgName
		}
		callInstance := createCallInstance(functionName, callArgs, op)
		if i > 0 {
			arg := args[i-1].Argument
			statements.Append(g.StatementList(
				g.IfStmt{
					Condition: g.Raw(jen.Id("args").Dot("noOfReadArguments").Op(">=").Lit(i)),
					Block: g.StatementList(
						wrappers.ReturnOnAnyError(errNames),
						callInstance,
					),
				}))
			if !arg.OptionalInGo() {
				statements.Append(
					g.Return(
						g.Nil,
						stdgen.ErrorsNew(g.Lit(missingArgsConts)),
					),
				)
				break
			}
		} else {
			statements.Append(wrappers.ReturnOnAnyError(errNames))
			statements.Append(callInstance)
		}
	}
	return statements
}

func V8RequireContext(wrapper WrapperInstance) g.Generator {
	info := v8ArgInfo(g.NewValue("info"))
	return g.Assign(
		g.Id("ctx"),
		wrapper.MustGetContext(info),
	)
}

type V8InstanceInvocation struct {
	Name     string
	Args     []g.Generator
	Op       ESOperation
	Instance *g.Value
	Receiver WrapperInstance
}

type V8InstanceInvocationResult struct {
	Generator      g.Generator
	HasValue       bool
	HasError       bool
	RequireContext bool
}

func (c V8InstanceInvocation) PerformCall() (genRes V8InstanceInvocationResult) {
	genRes.HasError = c.Op.GetHasError()
	genRes.HasValue = c.Op.HasResult() // != "undefined"
	var stmt *jen.Statement
	if genRes.HasValue {
		stmt = jen.Id("result")
	}
	if genRes.HasError {
		if stmt != nil {
			stmt = stmt.Op(",").Id("callErr")
		} else {
			stmt = jen.Id("callErr")
		}
	}
	if stmt != nil {
		stmt = stmt.Op(":=")
	}

	list := g.StatementListStmt{}
	var evaluation g.Value
	if c.Instance == nil {
		evaluation = g.NewValue(idlNameToGoName(c.Name)).Call(c.Args...)
	} else {
		evaluation = c.Instance.Method(idlNameToGoName(c.Name)).Call(c.Args...)
	}
	if stmt == nil {
		list.Append(evaluation)
	} else {
		list.Append(g.Raw(stmt.Add(evaluation.Generate())))
	}
	genRes.Generator = list
	return
}

func (c V8InstanceInvocation) GetGenerator() V8InstanceInvocationResult {
	genRes := c.PerformCall()
	list := g.StatementList()
	list.Append(genRes.Generator)
	if !genRes.HasValue {
		if genRes.HasError {
			list.Append(g.Return(g.Nil, g.Id("callErr")))
		} else {
			list.Append(g.Return(g.Nil, g.Nil))
		}
	} else {
		retType := c.Op.LegacyRetType
		if retType.IsNode() {
			genRes.RequireContext = true
			valueReturn := (g.Return(g.Raw(jen.Id("ctx").Dot("getInstanceForNode").Call(jen.Id("result")))))
			if genRes.HasError {
				list.Append(g.IfStmt{
					Condition: g.Neq{Lhs: g.Id("callErr"), Rhs: g.Nil},
					Block:     g.Return(g.Nil, g.Id("callErr")),
					Else:      valueReturn,
				})
			} else {
				list.Append(valueReturn)
			}
		} else {
			converter := c.Op.Encoder()
			genRes.RequireContext = true
			valueReturn := g.Return(c.Receiver.Method(converter).Call(g.Id("ctx"), g.Id("result")))
			if genRes.HasError {
				list.Append(g.IfStmt{
					Condition: g.Neq{Lhs: g.Id("callErr"), Rhs: g.Nil},
					Block:     g.Return(g.Nil, g.Id("callErr")),
					Else:      valueReturn,
				})
			} else {
				list.Append(valueReturn)
			}
		}
	}
	genRes.Generator = list
	return genRes
}

func CreateV8IllegalConstructorBody(data ESConstructorData) g.Generator {
	naming := V8NamingStrategy{data}
	return g.Return(g.Nil, g.NewValuePackage("NewTypeError", v8).
		Call(g.NewValue(naming.Receiver()).Field("scriptHost").Field("iso"),
			g.Lit("Illegal Constructor")))
}

type V8ReadArg struct {
	Argument ESOperationArgument
	ArgName  g.Generator
	ErrName  g.Generator
	Index    int
}

type V8ReadArguments struct {
	Args      []V8ReadArg
	Generator g.Generator
}

func (r V8ReadArguments) Generate() *jen.Statement {
	if r.Generator != nil {
		return r.Generator.Generate()
	} else {
		return g.Noop.Generate()
	}
}

func AssignArgs(data ESConstructorData, op ESOperation) g.Generator {
	if len(op.Arguments) == 0 {
		return g.Noop
	}
	naming := V8NamingStrategy{data}
	return g.Assign(
		g.Id("args"),
		g.NewValue("newArgumentHelper").Call(
			g.NewValue(naming.Receiver()).Field("scriptHost"),
			g.Id("info")),
	)
}

func ReadArguments(data ESConstructorData, op ESOperation) (res V8ReadArguments) {
	naming := V8NamingStrategy{data}
	argCount := len(op.Arguments)
	res.Args = make([]V8ReadArg, 0, argCount)
	statements := g.StatementList()
	for i, arg := range op.Arguments {
		argName := g.Id(wrappers.SanitizeVarName(arg.Name))
		errName := g.Id(fmt.Sprintf("err%d", i+1))
		if arg.Ignore {
			continue
		}
		res.Args = append(res.Args, V8ReadArg{
			Argument: arg,
			ArgName:  argName,
			ErrName:  errName,
			Index:    i,
		})

		var convertNames []string
		if arg.Type != "" {
			convertNames = []string{fmt.Sprintf("decode%s", idlNameToGoName(arg.Type))}
		} else {
			types := arg.IdlType.IdlType.IType.Types
			convertNames = make([]string, len(types))
			for i, t := range types {
				convertNames[i] = fmt.Sprintf("decode%s", t.IType.TypeName)
			}
		}

		gConverters := []g.Generator{g.Id("args"), g.Lit(i)}
		defaultName, hasDefault := arg.DefaultValueInGo()
		if hasDefault {
			gConverters = append(gConverters, g.NewValue(naming.Receiver()).Field(defaultName))
		}
		for _, n := range convertNames {
			gConverters = append(gConverters, g.NewValue(naming.Receiver()).Field(n))
		}
		if hasDefault {
			statements.Append(g.AssignMany(g.List(argName, errName),
				g.NewValue("tryParseArgWithDefault").Call(gConverters...)))
		} else if arg.IdlArg.Type.Nullable {
			statements.Append(g.AssignMany(
				g.List(argName, errName),
				g.NewValue("tryParseArgNullableType").Call(gConverters...)))
		} else {
			statements.Append(g.AssignMany(
				g.List(argName, errName),
				g.NewValue("tryParseArg").Call(gConverters...)))
		}
	}
	res.Generator = statements
	return
}

func GetInstanceAndError(id g.Generator, errId g.Generator, data ESConstructorData) g.Generator {
	naming := V8NamingStrategy{data}
	return g.AssignMany(
		g.List(id, errId),
		g.NewValue(naming.Receiver()).Field("getInstance").Call(g.Id("info")),
	)
}
