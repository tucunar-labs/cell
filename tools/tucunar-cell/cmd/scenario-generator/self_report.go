package main

import selfreport "tucunar-cell/internal/selfreport"

func withAttributes(attributes ...selfreport.Attribute) selfreport.StartOption {
	return selfreport.WithAttributes(attributes...)
}

var spanError = selfreport.Error

func spanString(key, value string) selfreport.Attribute  { return selfreport.String(key, value) }
func spanInt(key string, value int) selfreport.Attribute { return selfreport.Int(key, value) }
