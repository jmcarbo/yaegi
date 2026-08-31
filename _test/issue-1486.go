package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type TestObject struct {
	Name    string
	Surname string
}

type customTestObject struct {
	FullName string
}

func (t *TestObject) MarshalJSON() ([]byte, error) {
	custom := &customTestObject{FullName: fmt.Sprintf("%s %s", t.Name, t.Surname)}
	return json.Marshal(custom)
}

func (t *TestObject) UnmarshalJSON(b []byte) error {
	custom := &customTestObject{}
	json.Unmarshal(b, custom)
	fields := strings.Fields(custom.FullName)
	*t = TestObject{Name: fields[0], Surname: fields[1]}
	return nil
}

type CollectionOfTestObject struct {
	Collection []TestObject
}

func main() {
	testObject := &TestObject{Name: "Name", Surname: "Surname"}
	jsonBytes, err := json.Marshal(testObject)
	fmt.Println(string(jsonBytes), err == nil)
	col := CollectionOfTestObject{Collection: []TestObject{TestObject{"A", "B"}, TestObject{"C", "D"}}}
	b2, err2 := json.Marshal(col)
	fmt.Println(string(b2), err2 == nil)
	var col2 CollectionOfTestObject
	err3 := json.Unmarshal(b2, &col2)
	fmt.Println(col2.Collection[0].Name, col2.Collection[1].Surname, err3 == nil)
}
